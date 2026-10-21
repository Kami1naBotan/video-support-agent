import { describe, expect, it } from 'vitest'

import { decodeChatStreamEvent } from './chat-stream-event'
import type { ChatStreamEvent } from './chat-stream'

describe('chat stream event validation', () => {
    it('accepts all five event types', () => {
        const events: ChatStreamEvent[] = [
            {
                event: 'start',
                data: { conversation_id: 1 },
            },
            {
                event: 'token',
                data: { content: '  你好\n下一段  ' },
            },
            {
                event: 'citation',
                data: { items: [] },
            },
            {
                event: 'error',
                data: {
                    code: 'model_timeout',
                    message: '模型响应超时',
                },
            },
            {
                event: 'done',
                data: {
                    user_message_id: 10,
                    assistant_message_id: 11,
                },
            },
        ]

        for (const event of events) {
            const decoded = decodeChatStreamEvent({
                event: event.event,
                data: JSON.stringify(event.data),
            })

            expect(decoded).toEqual(event)
        }
    })

    it.each([
        { event: 'token', data: '{' },
        { event: 'token', data: 'null' },
        { event: 'token', data: '[]' },
        { event: 'token', data: '{"content":123}' },
        { event: 'start', data: '{"conversation_id":0}' },
        { event: 'start', data: '{"conversation_id":1.5}' },
        {
            event: 'start',
            data: '{"conversation_id":9007199254740992}',
        },
        { event: 'citation', data: '{"items":null}' },
        { event: 'error', data: '{"code":"","message":"失败"}' },
        { event: 'done', data: '{"user_message_id":10}' },
        { event: 'unexpected', data: '{}' },
    ])('rejects invalid frame: $event / $data', (frame) => {
        expect(() => decodeChatStreamEvent(frame)).toThrow()
    })

    it('accepts an empty token without changing its content', () => {
        expect(
            decodeChatStreamEvent({
                event: 'token',
                data: '{"content":""}',
            }),
        ).toEqual({
            event: 'token',
            data: { content: '' },
        })
    })
})