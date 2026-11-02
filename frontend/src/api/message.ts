import { http } from './http'
import { decodeRuleCitations, isRecord } from './rule-citation'
import type { RuleCitation } from './rule-citation'

export interface ChatMessage {
    id: number
    conversation_id: number
    role: string
    content: string
    created_at: string
    citations?: RuleCitation[]
}

export function decodeChatMessage(value: unknown): ChatMessage {
    if (
        !isRecord(value) ||
        typeof value.id !== 'number' ||
        !Number.isSafeInteger(value.id) ||
        value.id <= 0 ||
        typeof value.conversation_id !== 'number' ||
        !Number.isSafeInteger(value.conversation_id) ||
        value.conversation_id <= 0 ||
        typeof value.role !== 'string' ||
        value.role.trim() === '' ||
        typeof value.content !== 'string' ||
        typeof value.created_at !== 'string'
    ) {
        throw new Error('消息数据格式无效')
    }

    const citations =
        value.citations === undefined || value.citations === null
            ? []
            : decodeRuleCitations(value.citations)

    if (value.role !== 'assistant' && citations.length > 0) {
        throw new Error('非助手消息不能携带规则引用')
    }

    return {
        id: value.id,
        conversation_id: value.conversation_id,
        role: value.role,
        content: value.content,
        created_at: value.created_at,
        citations,
    }
}

export async function listMessagesAPI(
    conversationID: number,
    signal?: AbortSignal,
): Promise<ChatMessage[]> {
    const response = await http.get<unknown>(
        `/conversations/${conversationID}/messages`,
        { signal },
    )

    if (!Array.isArray(response.data)) {
        throw new Error('消息列表格式无效')
    }

    return response.data.map(decodeChatMessage)
}

export async function sendMessageAPI(
    conversationID: number,
    content: string,
    signal?: AbortSignal,
): Promise<ChatMessage> {
    const response = await http.post<unknown>(
        `/conversations/${conversationID}/messages`,
        { content },
        { signal },
    )

    return decodeChatMessage(response.data)
}