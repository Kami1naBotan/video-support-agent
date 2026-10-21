import { describe, expect, it } from 'vitest'

import { createSSEParser } from './sse-parser'
import type { SSEFrame } from './sse-parser'

describe('SSE parser', () => {
    it('parses events across every possible two-part split', () => {
        const text =
            'event: token\n' +
            'data: {"content":"你好"}\n\n' +
            'event: done\n' +
            'data: {"user_message_id":10,"assistant_message_id":11}\n\n'

        const expected: SSEFrame[] = [
            {
                event: 'token',
                data: '{"content":"你好"}',
            },
            {
                event: 'done',
                data: '{"user_message_id":10,"assistant_message_id":11}',
            },
        ]

        for (let split = 0; split <= text.length; split++) {
            const received: SSEFrame[] = []
            const parser = createSSEParser((frame) => {
                received.push(frame)
            })

            parser.push(text.slice(0, split))
            parser.push(text.slice(split))
            parser.finish()

            expect(received).toEqual(expected)
        }
    })

    it('supports fragmented CRLF and multiple data lines', () => {
        const received: SSEFrame[] = []
        const parser = createSSEParser((frame) => {
            received.push(frame)
        })

        parser.push(': heartbeat\r')
        parser.push('\nevent: token\r\ndata: first\r')
        parser.push('\ndata:  second\r\n\r')
        parser.push('\n')
        parser.finish()

        expect(received).toEqual([
            {
                event: 'token',
                data: 'first\n second',
            },
        ])
    })

    it('waits for the terminating blank line', () => {
        const received: SSEFrame[] = []
        const parser = createSSEParser((frame) => {
            received.push(frame)
        })

        parser.push('event: token\ndata: {"content":"你好"}\n')

        expect(received).toEqual([])

        parser.push('\n')
        parser.finish()

        expect(received).toEqual([
            {
                event: 'token',
                data: '{"content":"你好"}',
            },
        ])
    })

    it('rejects an unfinished event at EOF', () => {
        const parser = createSSEParser(() => {})

        parser.push('event: token\ndata: {"content":"你好"}\n')

        expect(() => parser.finish()).toThrow(
            'SSE 响应在事件结束前中断',
        )
    })
})