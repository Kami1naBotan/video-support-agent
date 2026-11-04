import { readToken } from '../auth/token'
import { createSSEParser } from './sse-parser'
import { decodeChatStreamEvent } from './chat-stream-event'
import {
    ChatStreamRequestError,
    decodeRequestError,
} from './chat-stream-error'
import type { RuleCitation } from './rule-citation'
import {
    encodeAgentRequest,
    type ActionRequired,
    type AgentDoneData,
    type AgentErrorData,
    type AgentMessageRequest,
    type AgentResult,
    type ToolEventData,
} from './agent-protocol'

export interface StreamStartData {
    conversation_id: number
}

export interface StreamTokenData {
    content: string
}

export interface StreamCitationData {
    items: RuleCitation[]
}

export type StreamErrorData = AgentErrorData

export interface LegacyStreamDoneData {
    user_message_id: number
    assistant_message_id: number
}

export type StreamDoneData = LegacyStreamDoneData | AgentDoneData

export type ChatStreamEvent =
    | { event: 'start'; data: StreamStartData }
    | { event: 'token'; data: StreamTokenData }
    | { event: 'citation'; data: StreamCitationData }
    | { event: 'error'; data: StreamErrorData }
    | { event: 'done'; data: StreamDoneData }
    | { event: 'agent_start'; data: { run_id: string } }
    | { event: 'tool_start'; data: ToolEventData }
    | { event: 'tool_end'; data: ToolEventData }
    | { event: 'action_required'; data: ActionRequired }
    | { event: 'agent_result'; data: AgentResult }

export interface StreamChatOptions {
    signal: AbortSignal
    onEvent: (event: ChatStreamEvent) => void
}

async function readHTTPError(
    response: Response,
): Promise<ChatStreamRequestError> {
    let data: unknown
    try {
        data = await response.json()
    } catch {
        return new ChatStreamRequestError(
            response.status,
            'http_error',
            `请求失败（HTTP ${response.status}），请先核对已有结果`,
        )
    }

    return decodeRequestError(response.status, data)
}

export async function streamAgentMessageAPI(
    conversationID: number,
    request: AgentMessageRequest,
    options: StreamChatOptions,
): Promise<StreamDoneData> {
    if (
        !Number.isSafeInteger(conversationID) ||
        conversationID <= 0
    ) {
        throw new Error('会话 ID 无效')
    }

    const body = encodeAgentRequest(request)

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
            body: JSON.stringify(body),
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

    let activeRunID: string | undefined
    let receivedAgentResult = false

    function checkAgentEvent(event: ChatStreamEvent): void {
        if (event.event === 'agent_start') {
            if (activeRunID !== undefined) {
                throw new Error('重复收到 agent_start')
            }
            activeRunID = event.data.run_id
            return
        }

        if (
            event.event === 'tool_start' ||
            event.event === 'tool_end' ||
            event.event === 'action_required' ||
            event.event === 'agent_result'
        ) {
            if (
                activeRunID === undefined ||
                event.data.run_id !== activeRunID
            ) {
                throw new Error('Agent 事件未关联当前 Run')
            }

            if (receivedAgentResult) {
                throw new Error('agent_result 后仍收到执行过程事件')
            }
            if (event.event === 'agent_result') {
                receivedAgentResult = true
            }
            return
        }

        if (event.event === 'done') {
            if ('agent' in event.data) {
                if (
                    activeRunID === undefined ||
                    event.data.run_id !== activeRunID ||
                    !receivedAgentResult
                ) {
                    throw new Error('Agent done 缺少匹配的执行结果')
                }
            } else if (activeRunID !== undefined) {
                throw new Error('Agent 流不能使用旧版 done 收尾')
            }
            return
        }

        if (event.event === 'error') {
            const ids = [event.data.run_id, event.data.agent?.run_id]
            for (const id of ids) {
                if (id !== undefined && id !== activeRunID) {
                    throw new Error('错误事件未关联当前 Run')
                }
            }
            return
        }

        if (
            activeRunID !== undefined &&
            (event.event === 'token' || event.event === 'citation') &&
            !receivedAgentResult
        ) {
            throw new Error('Agent 输出早于 agent_result')
        }
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

        checkAgentEvent(event)

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
                event.data,
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

export async function streamMessageAPI(
    conversationID: number,
    content: string,
    options: StreamChatOptions,
): Promise<StreamDoneData> {
    return streamAgentMessageAPI(
        conversationID,
        { content },
        options,
    )
}