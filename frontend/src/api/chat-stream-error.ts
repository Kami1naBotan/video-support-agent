import {
    decodeAgentError,
    type AgentErrorData,
} from './agent-protocol'

export class ChatStreamRequestError extends Error {
    readonly status: number
    readonly code: string
    readonly details: AgentErrorData | undefined

    constructor(
        status: number,
        code: string,
        message: string,
        details?: AgentErrorData,
    ) {
        super(message)
        this.name = 'ChatStreamRequestError'
        this.status = status
        this.code = code
        this.details = details
    }
}

export function decodeRequestError(
    status: number,
    value: unknown,
): ChatStreamRequestError {
    try {
        const details = decodeAgentError(value)
        return new ChatStreamRequestError(
            status,
            details.code,
            details.message,
            details,
        )
    } catch {
        return new ChatStreamRequestError(
            status,
            'invalid_response',
            '错误响应未通过协议校验，请先查询恢复结果，不要自动重复提交',
        )
    }
}