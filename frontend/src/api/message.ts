import { http } from './http'

export interface ChatMessage {
    id: number
    conversation_id: number
    role: string
    content: string
    created_at: string
}

export async function listMessagesAPI(
    conversationID: number,
    signal?: AbortSignal,
): Promise<ChatMessage[]> {
    const response = await http.get<ChatMessage[]>(
        `/conversations/${conversationID}/messages`,
        { signal },
    )

    return response.data
}

export async function sendMessageAPI(
    conversationID: number,
    content: string,
    signal?: AbortSignal,
): Promise<ChatMessage> {
    const response = await http.post<ChatMessage>(
        `/conversations/${conversationID}/messages`,
        { content },
        { signal },
    )

    return response.data
}