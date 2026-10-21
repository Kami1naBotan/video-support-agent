import { readToken } from '../auth/token'
import { createSSEParser } from './sse-parser'
import { decodeChatStreamEvent } from './chat-stream-event'
import { ChatStreamRequestError } from './chat-stream-error'

export interface StreamStartData {
    conversation_id: number
}

export interface StreamTokenData {
    content: string
}

export interface StreamCitationData {
    items: unknown[]
}

export interface StreamErrorData {
    code: string
    message: string
}

export interface StreamDoneData {
    user_message_id: number
    assistant_message_id: number
}

export type ChatStreamEvent =
    | { event: 'start'; data: StreamStartData }
    | { event: 'token'; data: StreamTokenData }
    | { event: 'citation'; data: StreamCitationData }
    | { event: 'error'; data: StreamErrorData }
    | { event: 'done'; data: StreamDoneData }

export interface StreamChatOptions {
    signal: AbortSignal
    onEvent: (event: ChatStreamEvent) => void
}

async function readHTTPError(
    response: Response,
): Promise<ChatStreamRequestError> {
    let code = 'http_error'
    let message = `请求失败（HTTP ${response.status}）`

    try {
        const data: unknown = await response.json()

        if (
            typeof data === 'object' &&
            data !== null &&
            !Array.isArray(data)
        ) {
            const body = data as Record<string, unknown>

            if (typeof body.code === 'string' && body.code.trim() !== '') {
                code = body.code
            }
            if (
                typeof body.message === 'string' &&
                body.message.trim() !== ''
            ) {
                message = body.message
            }
        }
    } catch {
        // 例如 Nginx 返回 HTML 错误页，保留通用 HTTP 提示。
    }

    return new ChatStreamRequestError(response.status, code, message)
}

export async function streamMessageAPI(
    conversationID: number,
    content: string,
    options: StreamChatOptions,
): Promise<StreamDoneData> {
    if (
        !Number.isSafeInteger(conversationID) ||
        conversationID <= 0
    ) {
        throw new Error('会话 ID 无效')
    }

    // 用户取消与请求总超时，任意一个触发都会中止请求。
    const signal = AbortSignal.any([
        options.signal,
        AbortSignal.timeout(60_000),
    ])

    signal.throwIfAborted()

    const token = readToken()
    const headers: Record<string, string> = {
        'Content-Type': 'application/json',
        Accept: 'text/event-stream',
    }

    if (token !== '') {
        headers.Authorization = `Bearer ${token}`
    }

    const response = await fetch(
        `/api/v1/conversations/${conversationID}/messages/stream`,
        {
            method: 'POST',
            headers,
            body: JSON.stringify({ content }),
            signal,
        },
    )

    if (!response.ok) {
        const error = await readHTTPError(response)
        signal.throwIfAborted()
        throw error
    }

    const contentType = response.headers
        .get('Content-Type')
        ?.split(';')[0]
        ?.trim()
        .toLowerCase()

    if (contentType !== 'text/event-stream') {
        await response.body?.cancel().catch(() => {})
        throw new Error('服务端未返回 SSE 响应')
    }

    if (response.body === null) {
        throw new Error('服务端未返回响应流')
    }

    const reader = response.body.getReader()
    const decoder = new TextDecoder('utf-8', { fatal: true })

    const state: {
        started: boolean
        terminal: boolean
        done: StreamDoneData | null
    } = {
        started: false,
        terminal: false,
        done: null,
    }

    const parser = createSSEParser((frame) => {
        signal.throwIfAborted()

        if (state.terminal) {
            throw new Error('终止事件之后仍收到业务事件')
        }

        const event = decodeChatStreamEvent(frame)

        if (!state.started) {
            if (event.event !== 'start') {
                throw new Error('首个流式事件必须是 start')
            }
            if (event.data.conversation_id !== conversationID) {
                throw new Error('流式响应的会话 ID 与请求不一致')
            }

            state.started = true
        } else if (event.event === 'start') {
            throw new Error('重复收到 start 事件')
        }

        if (event.event === 'done') {
            state.terminal = true
            state.done = event.data
        } else if (event.event === 'error') {
            state.terminal = true
        }

        options.onEvent(event)

        if (event.event === 'error') {
            throw new ChatStreamRequestError(
                response.status,
                event.data.code,
                event.data.message,
            )
        }
    })

    try {
        while (true) {
            signal.throwIfAborted()

            const { value, done } = await reader.read()

            signal.throwIfAborted()

            if (done) {
                // 处理解码器中可能剩余的字节。
                parser.push(decoder.decode())
                parser.finish()

                if (state.done !== null) {
                    return state.done
                }

                throw new Error('连接已结束，但未收到完成事件，请刷新历史确认')
            }

            parser.push(decoder.decode(value, { stream: true }))

            if (state.done !== null) {
                // done 已确认落库，不需要继续等待服务器关闭连接。
                parser.finish()
                return state.done
            }
        }
    } finally {
        try {
            await reader.cancel()
        } catch {
            // 连接可能已取消或出错，避免清理异常覆盖原始错误。
        } finally {
            reader.releaseLock()
        }
    }
}