// @vitest-environment node

import {
    afterEach,
    beforeEach,
    describe,
    expect,
    it,
    vi,
} from 'vitest'

import { streamMessageAPI } from './chat-stream'
import { ChatStreamRequestError } from './chat-stream-error'
import type { ChatStreamEvent } from './chat-stream'

vi.mock('../auth/token', () => ({
    readToken: () => 'test-only-token',
}))

const fetchMock = vi.fn<typeof fetch>()

beforeEach(() => {
    fetchMock.mockReset()
    vi.stubGlobal('fetch', fetchMock)
})

afterEach(() => {
    vi.unstubAllGlobals()
})

function makeSSETestResponse(events: ChatStreamEvent[]): Response {
    const text = events
        .map((event) =>
            `event: ${event.event}\ndata: ${JSON.stringify(event.data)}\n\n`,
        )
        .join('')

    const body = new ReadableStream<Uint8Array>({
        start(controller) {
            controller.enqueue(new TextEncoder().encode(text))
            controller.close()
        },
    })

    return new Response(body, {
        status: 200,
        headers: {
            'Content-Type': 'text/event-stream',
        },
    })
}

describe('streamMessageAPI', () => {
    it('sends authentication and decodes byte-split Chinese text', async () => {
        const expectedEvents: ChatStreamEvent[] = [
            {
                event: 'start',
                data: { conversation_id: 1 },
            },
            {
                event: 'token',
                data: { content: '你好' },
            },
            {
                event: 'token',
                data: { content: '\n第二段 🙂' },
            },
            {
                event: 'citation',
                data: { items: [] },
            },
            {
                event: 'done',
                data: {
                    user_message_id: 10,
                    assistant_message_id: 11,
                },
            },
        ]

        const wireText = expectedEvents
            .map((event) =>
                `event: ${event.event}\ndata: ${JSON.stringify(event.data)}\n\n`,
            )
            .join('')

        const bytes = new TextEncoder().encode(wireText)
        const cancelStream = vi.fn()

        const body = new ReadableStream<Uint8Array>({
            start(controller) {
                // 每次只提供一个字节，故意拆开中文和 emoji。
                for (const byte of bytes) {
                    controller.enqueue(Uint8Array.of(byte))
                }

                // 不关闭流，验证收到 done 后无需等待 EOF。
            },
            cancel: cancelStream,
        })

        fetchMock.mockResolvedValue(
            new Response(body, {
                status: 200,
                headers: {
                    'Content-Type': 'text/event-stream; charset=utf-8',
                },
            }),
        )

        const controller = new AbortController()
        const received: ChatStreamEvent[] = []

        const result = await streamMessageAPI(
            1,
            '我的问题',
            {
                signal: controller.signal,
                onEvent: (event) => {
                    received.push(event)
                },
            },
        )

        expect(fetchMock).toHaveBeenCalledTimes(1)

        expect(fetchMock).toHaveBeenCalledWith(
            '/api/v1/conversations/1/messages/stream',
            expect.objectContaining({
                method: 'POST',
                headers: {
                    'Content-Type': 'application/json',
                    Accept: 'text/event-stream',
                    Authorization: 'Bearer test-only-token',
                },
                body: JSON.stringify({ content: '我的问题' }),
                signal: expect.any(AbortSignal),
            }),
        )

        expect(received).toEqual(expectedEvents)

        expect(result).toEqual({
            user_message_id: 10,
            assistant_message_id: 11,
        })

        expect(cancelStream).toHaveBeenCalledTimes(1)
        expect(body.locked).toBe(false)
    })

    it('rejects HTTP 401 without delivering SSE events', async () => {
        fetchMock.mockResolvedValue(
            new Response(
                JSON.stringify({
                    code: 'unauthorized',
                    message: '登录凭证无效或已过期',
                }),
                {
                    status: 401,
                    headers: {
                        'Content-Type': 'application/json',
                    },
                },
            ),
        )

        const onEvent = vi.fn()

        const request = streamMessageAPI(1, '我的问题', {
            signal: new AbortController().signal,
            onEvent,
        })

        await expect(request).rejects.toBeInstanceOf(ChatStreamRequestError)

        await expect(request).rejects.toMatchObject({
            status: 401,
            code: 'unauthorized',
            message: '登录凭证无效或已过期',
        })

        expect(onEvent).not.toHaveBeenCalled()
    })

    it('rejects an SSE error even when HTTP status is 200', async () => {
        const events: ChatStreamEvent[] = [
            {
                event: 'start',
                data: { conversation_id: 1 },
            },
            {
                event: 'token',
                data: { content: '尚未完成的回答' },
            },
            {
                event: 'error',
                data: {
                    code: 'model_timeout',
                    message: '模型响应超时，请稍后重试',
                },
            },
        ]

        const response = makeSSETestResponse(events)
        fetchMock.mockResolvedValue(response)

        const received: ChatStreamEvent[] = []

        const request = streamMessageAPI(1, '我的问题', {
            signal: new AbortController().signal,
            onEvent: (event) => {
                received.push(event)
            },
        })

        await expect(request).rejects.toBeInstanceOf(ChatStreamRequestError)

        await expect(request).rejects.toMatchObject({
            status: 200,
            code: 'model_timeout',
            message: '模型响应超时，请稍后重试',
        })

        expect(received).toEqual(events)
        expect(received.some((event) => event.event === 'done')).toBe(false)
        expect(response.body?.locked).toBe(false)
    })

    it('rejects EOF when no terminal event was received', async () => {
        const events: ChatStreamEvent[] = [
            {
                event: 'start',
                data: { conversation_id: 1 },
            },
            {
                event: 'token',
                data: { content: '页面已经收到的文字' },
            },
        ]

        const response = makeSSETestResponse(events)
        fetchMock.mockResolvedValue(response)

        const received: ChatStreamEvent[] = []

        await expect(
            streamMessageAPI(1, '我的问题', {
                signal: new AbortController().signal,
                onEvent: (event) => {
                    received.push(event)
                },
            }),
        ).rejects.toThrow(
            '连接已结束，但未收到完成事件，请刷新历史确认',
        )

        expect(received).toEqual(events)
        expect(response.body?.locked).toBe(false)
    })

    it('aborts while waiting for the next chunk and releases the reader', async () => {
        const controller = new AbortController()
        const received: ChatStreamEvent[] = []
        const transportAborted = vi.fn()

        // 用明确的信号同步测试，不依赖 setTimeout 猜测执行进度。
        let notifyWaiting!: () => void
        const waitingForNextChunk = new Promise<void>((resolve) => {
            notifyWaiting = resolve
        })

        let responseBody: ReadableStream<Uint8Array> | undefined
        let removeAbortListener = () => {}

        fetchMock.mockImplementation(async (_input, init) => {
            // 必须使用 API 传给 fetch 的组合 signal。
            const signal = init?.signal
            if (!signal) {
                throw new Error('fetch 缺少取消信号')
            }

            const initialEvents: ChatStreamEvent[] = [
                {
                    event: 'start',
                    data: { conversation_id: 1 },
                },
                {
                    event: 'token',
                    data: { content: '已经生成的部分回答' },
                },
            ]

            const text = initialEvents
                .map((event) =>
                    `event: ${event.event}\ndata: ${JSON.stringify(event.data)}\n\n`,
                )
                .join('')

            responseBody = new ReadableStream<Uint8Array>(
                {
                    start(streamController) {
                        const onAbort = () => {
                            transportAborted()
                            // 模拟原生 Fetch：取消使待处理的 read() 拒绝。
                            streamController.error(signal.reason)
                        }

                        signal.addEventListener('abort', onAbort, { once: true })
                        removeAbortListener = () => {
                            signal.removeEventListener('abort', onAbort)
                        }

                        if (signal.aborted) {
                            onAbort()
                            return
                        }

                        streamController.enqueue(
                            new TextEncoder().encode(text),
                        )
                        // 保持连接打开，不发送 done，也不主动关闭。
                    },

                    pull() {
                        // 第一块已读完，现在客户端正在请求下一块。
                        notifyWaiting()
                    },

                    cancel() {
                        removeAbortListener()
                    },
                },
                {
                    // 禁止提前拉取，确保 pull 对应实际的读取需求。
                    highWaterMark: 0,
                },
            )

            return new Response(responseBody, {
                status: 200,
                headers: {
                    'Content-Type': 'text/event-stream',
                },
            })
        })

        const request = streamMessageAPI(1, '我的问题', {
            signal: controller.signal,
            onEvent: (event) => {
                received.push(event)
            },
        })

        // 先注册拒绝断言，再触发取消，避免未处理的 Promise rejection。
        const rejected = expect(request).rejects.toMatchObject({
            name: 'AbortError',
        })

        try {
            await waitingForNextChunk

            expect(received.map((event) => event.event)).toEqual([
                'start',
                'token',
            ])

            controller.abort()

            await rejected

            expect(transportAborted).toHaveBeenCalledTimes(1)
            expect(received.some((event) => event.event === 'done')).toBe(false)
            expect(responseBody).toBeDefined()
            expect(responseBody?.locked).toBe(false)
        } finally {
            controller.abort()
            removeAbortListener()
        }
    })
})