import type { ChatStreamEvent } from './chat-stream'
import type { SSEFrame } from './sse-parser'
import { decodeRuleCitations } from './rule-citation'
import {
    decodeActionRequired,
    decodeAgentDone,
    decodeAgentError,
    decodeAgentResult,
    decodeMessageReceipt,
    decodeToolEvent,
    uuidValue,
} from './agent-protocol'

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
            return {
                event: 'citation',
                data: { items: decodeRuleCitations(data.items) },
            }

        case 'error':
            return {
                event: 'error',
                data: decodeAgentError(data),
            }

        case 'done':
            if (
                data.run_id !== undefined ||
                data.agent !== undefined ||
                data.message_save_status !== undefined
            ) {
                return {
                    event: 'done',
                    data: decodeAgentDone(data),
                }
            }

            return {
                event: 'done',
                data: decodeMessageReceipt(data),
            }

        case 'agent_start':
            return {
                event: 'agent_start',
                data: { run_id: uuidValue(data.run_id) },
            }

        case 'tool_start':
            return {
                event: 'tool_start',
                data: decodeToolEvent(data, false),
            }

        case 'tool_end':
            return {
                event: 'tool_end',
                data: decodeToolEvent(data, true),
            }

        case 'action_required':
            return {
                event: 'action_required',
                data: decodeActionRequired(data),
            }

        case 'agent_result':
            return {
                event: 'agent_result',
                data: decodeAgentResult(data),
            }

        default:
            throw new Error('收到不支持的流式事件')
    }
}