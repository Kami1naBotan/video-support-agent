import axios from 'axios'
import { describe, expect, it } from 'vitest'

import { ChatStreamRequestError } from './chat-stream-error'
import {
    errorMessage,
    isCanceled,
    isUnauthorized,
} from './errors'

describe('stream error handling', () => {
    it('recognizes cancellation without treating timeout as cancellation', () => {
        const canceled = new DOMException('aborted', 'AbortError')
        const timeout = new DOMException('timed out', 'TimeoutError')

        expect(isCanceled(canceled)).toBe(true)
        expect(isCanceled(new axios.CanceledError())).toBe(true)
        expect(isCanceled(timeout)).toBe(false)

        expect(errorMessage(timeout, '发送失败')).toBe(
            '等待回答超时，请刷新消息确认结果',
        )
    })

    it('recognizes HTTP 401 from the streaming API', () => {
        const error = new ChatStreamRequestError(
            401,
            'unauthorized',
            'authentication required',
        )

        expect(isUnauthorized(error)).toBe(true)
        expect(errorMessage(error, '发送失败')).toBe(
            '登录凭证无效或已过期',
        )
    })

    it('shows a model error carried by an HTTP 200 SSE response', () => {
        const error = new ChatStreamRequestError(
            200,
            'model_timeout',
            '模型响应超时，请稍后重试',
        )

        expect(isCanceled(error)).toBe(false)
        expect(isUnauthorized(error)).toBe(false)
        expect(errorMessage(error, '发送失败')).toBe(
            '模型响应超时，请稍后重试',
        )
    })

    it('keeps unknown errors behind the supplied fallback', () => {
        expect(
            errorMessage(
                new TypeError('Failed to fetch'),
                '连接异常，请刷新消息确认结果',
            ),
        ).toBe('连接异常，请刷新消息确认结果')
    })
})