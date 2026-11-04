// @vitest-environment node

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { streamAgentMessageAPI } from './chat-stream'
import { decodeConfirmedAction } from './agent-protocol'
import {
    actionRequired, agentDone, agentResult, confirmedAction,
    otherRunID, runID, toolEvent,
} from '../test/agent-fixtures'

vi.mock('../auth/token', () => ({
    readToken: () => 'fixture-bearer',
}))

const fetchMock = vi.fn<typeof fetch>()
type Frame = [string, unknown]

beforeEach(() => {
    fetchMock.mockReset()
    vi.stubGlobal('fetch', fetchMock)
})

afterEach(() => {
    vi.unstubAllGlobals()
})

function prefix(): Frame[] {
    return [
        ['start', { conversation_id: 1 }],
        ['agent_start', { run_id: runID }],
    ]
}

function successFrames(): Frame[] {
    return [
        ...prefix(),
        ['tool_start', toolEvent(false)],
        ['tool_end', toolEvent(true)],
        ['agent_result', agentResult()],
        ['token', { content: '你好 🙂' }],
        ['citation', { items: [] }],
        ['done', agentDone()],
    ]
}

function wire(frames: Frame[]): string {
    return frames.map(([event, data]) =>
        `event: ${event}\ndata: ${JSON.stringify(data)}\n\n`,
    ).join('')
}

function responseFor(
    frames: Frame[],
    byteSplit = false,
    close = false,
) {
    const cancel = vi.fn()
    const bytes = new TextEncoder().encode(wire(frames))
    const body = new ReadableStream<Uint8Array>({
        start(controller) {
            if (byteSplit) {
                for (const byte of bytes) controller.enqueue(Uint8Array.of(byte))
            } else {
                controller.enqueue(bytes)
            }
            if (close) controller.close()
        },
        cancel,
    })
    const response = new Response(body, {
        headers: { 'Content-Type': 'text/event-stream; charset=utf-8' },
    })
    return { body, response, cancel }
}

function options(onEvent = vi.fn()) {
    return { signal: new AbortController().signal, onEvent }
}

describe('Agent stream transport', () => {
    it('handles full byte-split Agent sequence', async () => {
        const frames = successFrames()
        frames.splice(4, 0, ['action_required', actionRequired()])
        frames[5] = ['agent_result', agentResult({ decision: 'confirmation_required' })]
        frames[8] = ['done', {
            ...agentDone(),
            agent: agentResult({
                decision: 'confirmation_required',
                message_save_status: 'saved',
            }),
        }]

        const fixture = responseFor(frames, true)
        fetchMock.mockResolvedValueOnce(fixture.response)
        const onEvent = vi.fn()
        const action = decodeConfirmedAction(confirmedAction())
        const done = await streamAgentMessageAPI(
            1, { confirmed_action: action }, options(onEvent),
        )

        expect(done.assistant_message_id).toBe(11)
        expect(onEvent.mock.calls.map(([event]) => event.event)).toEqual(
            frames.map(([name]) => name),
        )
        expect(onEvent.mock.calls.find(([event]) => event.event === 'token')?.[0].data)
            .toEqual({ content: '你好 🙂' })
        expect(fetchMock).toHaveBeenCalledWith(
            '/api/v1/conversations/1/messages/stream',
            expect.objectContaining({
                method: 'POST',
                body: JSON.stringify({ confirmed_action: action }),
                headers: expect.objectContaining({
                    Authorization: 'Bearer fixture-bearer',
                }),
            }),
        )
        expect(fixture.cancel).toHaveBeenCalledTimes(1)
        expect(fixture.body.locked).toBe(false)
    })

    it('rejects invalid event ordering', async () => {
        const invalid: Frame[][] = [
            [['agent_start', { run_id: runID }]],
            [['start', { conversation_id: 2 }]],
            [...prefix(), ['agent_start', { run_id: runID }]],
            [...prefix(), ['tool_start', { ...toolEvent(false), run_id: otherRunID }]],
            [...prefix(), ['token', { content: 'too early' }]],
            [...prefix(), ['done', agentDone()]],
            [...prefix(), ['agent_result', agentResult()], ['tool_start', toolEvent(false)]],
            [...prefix(), ['agent_result', agentResult()], [
                'done', { user_message_id: 10, assistant_message_id: 11 },
            ]],
            [...successFrames(), ['token', { content: 'after done' }]],
        ]
        for (const frames of invalid) {
            const fixture = responseFor(frames)
            fetchMock.mockResolvedValueOnce(fixture.response)
            await expect(streamAgentMessageAPI(
                1, { content: 'hello' }, options(),
            )).rejects.toThrow()
            expect(fixture.cancel).toHaveBeenCalledTimes(1)
            expect(fixture.body.locked).toBe(false)
        }
    })

    it('preserves terminal business errors', async () => {
        const error = {
            code: 'agent_message_save_unconfirmed',
            message: '保存待确认',
            run_id: runID,
            agent: agentResult({
                business_effect: 'committed',
                message_save_status: 'unknown',
            }),
        }
        const fixture = responseFor([...prefix(), ['error', error]])
        fetchMock.mockResolvedValueOnce(fixture.response)
        const onEvent = vi.fn()

        await expect(streamAgentMessageAPI(
            1, { content: 'hello' }, options(onEvent),
        )).rejects.toMatchObject({
            status: 200,
            code: error.code,
            details: {
                agent: { business_effect: 'committed', message_save_status: 'unknown' },
            },
        })
        expect(onEvent.mock.calls.map(([event]) => event.event)).toEqual([
            'start', 'agent_start', 'error',
        ])
        expect(fixture.cancel).toHaveBeenCalledTimes(1)
        expect(fixture.body.locked).toBe(false)
    })

    it('releases reader after callback failure', async () => {
        const fixture = responseFor(successFrames())
        fetchMock.mockResolvedValueOnce(fixture.response)
        const failure = new Error('fixture render failure')

        await expect(streamAgentMessageAPI(1, { content: 'hello' }, {
            signal: new AbortController().signal,
            onEvent(event) {
                if (event.event === 'tool_start') throw failure
            },
        })).rejects.toBe(failure)
        expect(fixture.cancel).toHaveBeenCalledTimes(1)
        expect(fixture.body.locked).toBe(false)
    })

    it('rejects missing done and invalid UTF8', async () => {
        const ended = responseFor(prefix(), false, true)
        fetchMock.mockResolvedValueOnce(ended.response)
        await expect(streamAgentMessageAPI(
            1, { content: 'hello' }, options(),
        )).rejects.toThrow('未收到完成事件')
        expect(ended.body.locked).toBe(false)

        const cancel = vi.fn()
        const invalid = new ReadableStream<Uint8Array>({
            start(controller) {
                controller.enqueue(Uint8Array.of(0xff))
            },
            cancel,
        })
        fetchMock.mockResolvedValueOnce(new Response(invalid, {
            headers: { 'Content-Type': 'text/event-stream' },
        }))
        await expect(streamAgentMessageAPI(
            1, { content: 'hello' }, options(),
        )).rejects.toThrow()
        expect(cancel).toHaveBeenCalledTimes(1)
        expect(invalid.locked).toBe(false)
    })

    it('rejects HTTP failures and cancels wrong content type', async () => {
        fetchMock.mockResolvedValueOnce(new Response(JSON.stringify({
            code: 'agent_message_save_unconfirmed',
            message: '保存待确认',
            agent: agentResult({ business_effect: 'committed' }),
        }), {
            status: 503,
            headers: { 'Content-Type': 'application/json' },
        }))
        const onEvent = vi.fn()
        await expect(streamAgentMessageAPI(
            1, { content: 'hello' }, options(onEvent),
        )).rejects.toMatchObject({
            status: 503,
            details: { agent: { business_effect: 'committed' } },
        })
        expect(onEvent).not.toHaveBeenCalled()

        const cancel = vi.fn()
        const body = new ReadableStream<Uint8Array>({ cancel })
        fetchMock.mockResolvedValueOnce(new Response(body, {
            headers: { 'Content-Type': 'application/json' },
        }))
        await expect(streamAgentMessageAPI(
            1, { content: 'hello' }, options(),
        )).rejects.toThrow('SSE')
        expect(cancel).toHaveBeenCalledTimes(1)
        expect(body.locked).toBe(false)

        fetchMock.mockClear()
        const controller = new AbortController()
        controller.abort()
        await expect(streamAgentMessageAPI(1, { content: 'hello' }, {
            signal: controller.signal, onEvent,
        })).rejects.toMatchObject({ name: 'AbortError' })
        expect(fetchMock).not.toHaveBeenCalled()
    })
})