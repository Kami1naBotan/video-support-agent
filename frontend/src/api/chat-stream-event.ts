import type { ChatStreamEvent } from './chat-stream'
import type { SSEFrame } from './sse-parser'

function isObject(
    value: unknown,
): value is Record<string, unknown> {
    return (
        typeof value === 'object' &&
        value !== null &&
        !Array.isArray(value)
    )
}

function isPositiveID(value: unknown): value is number {
    return (
        typeof value === 'number' &&
        Number.isSafeInteger(value) &&
        value > 0
    )
}

export function decodeChatStreamEvent(
    frame: SSEFrame,
): ChatStreamEvent {
    let data: unknown

    try {
        data = JSON.parse(frame.data)
    } catch {
        throw new Error('流式事件包含无效 JSON')
    }

    if (!isObject(data)) {
        throw new Error('流式事件数据必须是对象')
    }

    switch (frame.event) {
        case 'start':
            if (!isPositiveID(data.conversation_id)) {
                throw new Error('start 事件的会话 ID 无效')
            }

            return {
                event: 'start',
                data: { conversation_id: data.conversation_id },
            }

        case 'token':
            if (typeof data.content !== 'string') {
                throw new Error('token 事件的内容必须是字符串')
            }

            return {
                event: 'token',
                data: { content: data.content },
            }

        case 'citation':
            if (!Array.isArray(data.items)) {
                throw new Error('citation 事件的引用必须是数组')
            }

            return {
                event: 'citation',
                data: { items: data.items },
            }

        case 'error':
            if (
                typeof data.code !== 'string' ||
                data.code.trim() === '' ||
                typeof data.message !== 'string' ||
                data.message.trim() === ''
            ) {
                throw new Error('error 事件缺少有效错误码或提示')
            }

            return {
                event: 'error',
                data: {
                    code: data.code,
                    message: data.message,
                },
            }

        case 'done':
            if (
                !isPositiveID(data.user_message_id) ||
                !isPositiveID(data.assistant_message_id)
            ) {
                throw new Error('done 事件的消息 ID 无效')
            }

            return {
                event: 'done',
                data: {
                    user_message_id: data.user_message_id,
                    assistant_message_id: data.assistant_message_id,
                },
            }

        default:
            throw new Error('收到不支持的流式事件')
    }
}