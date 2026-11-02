import { beforeEach, describe, expect, it, vi } from 'vitest'

import { http } from './http'
import {
    decodeChatMessage,
    listMessagesAPI,
    sendMessageAPI,
} from './message'
import { citation, message } from '../test/rag-fixtures'

vi.mock('./http', () => ({
    http: { get: vi.fn(), post: vi.fn() },
}))

beforeEach(() => {
    vi.resetAllMocks()
})

describe('message API', () => {
    it('legacy and cited messages', () => {
        const legacy: Record<string, unknown> = { ...message() }
        delete legacy.citations

        expect(decodeChatMessage(legacy).citations).toEqual([])
        expect(decodeChatMessage({
            ...legacy, citations: null,
        }).citations).toEqual([])

        const input = message({ citations: [citation()] })
        const result = decodeChatMessage(input)
        expect(result).toEqual(input)
        expect(result).not.toBe(input)
        expect(result.citations).not.toBe(input.citations)

        input.citations![0]!.chunk_ids.push('999')
        expect(result.citations![0]!.chunk_ids).toEqual(['101'])

        expect(decodeChatMessage(message({
            role: 'user',
            content: '<em>普通文本</em>',
        })).content).toBe('<em>普通文本</em>')
    })

    it('malformed messages', () => {
        const bad: unknown[] = [
            null, [], {},
            message({ id: 0 }),
            message({ id: 1.5 }),
            message({ id: Number.MAX_SAFE_INTEGER + 1 }),
            message({ conversation_id: 0 }),
            message({ conversation_id: Number.MAX_SAFE_INTEGER + 1 }),
            message({ role: ' ' }),
            { ...message(), content: null },
            { ...message(), created_at: 1 },
            { ...message(), citations: {} },
            message({ role: 'user', citations: [citation()] }),
            message({ citations: [{ ...citation(), document_id: '0' }] }),
        ]

        for (const value of bad) {
            expect(() => decodeChatMessage(value)).toThrow()
        }
    })

    it('list request', async () => {
        const signal = new AbortController().signal
        const saved = message({ citations: [citation()] })
        vi.mocked(http.get).mockResolvedValueOnce({ data: [saved] })

        expect(await listMessagesAPI(1, signal)).toEqual([saved])
        expect(http.get).toHaveBeenCalledExactlyOnceWith(
            '/conversations/1/messages',
            { signal },
        )

        for (const data of [null, {}, [saved, { ...saved, id: 0 }]]) {
            vi.mocked(http.get).mockResolvedValueOnce({ data })
            await expect(listMessagesAPI(1, signal)).rejects.toThrow()
        }
    })

    it('send request', async () => {
        const signal = new AbortController().signal
        const savedUser = message({
            id: 10,
            role: 'user',
            content: '虚构问题',
        })
        vi.mocked(http.post).mockResolvedValueOnce({ data: savedUser })

        expect(await sendMessageAPI(1, '虚构问题', signal)).toEqual(savedUser)
        expect(http.post).toHaveBeenCalledExactlyOnceWith(
            '/conversations/1/messages',
            { content: '虚构问题' },
            { signal },
        )

        vi.mocked(http.post).mockResolvedValueOnce({
            data: { ...savedUser, id: 0 },
        })
        await expect(sendMessageAPI(1, '虚构问题', signal)).rejects.toThrow()

        const error = new Error('test-only request failure')
        vi.mocked(http.post).mockRejectedValueOnce(error)
        await expect(sendMessageAPI(1, '虚构问题', signal)).rejects.toBe(error)
    })
})