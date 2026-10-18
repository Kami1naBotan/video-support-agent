import { http } from './http'

export interface Conversation {
    id: number
    user_id: number
    title: string
    created_at: string
    updated_at: string
}

export async function listConversationsAPI(
    signal?: AbortSignal,
): Promise<Conversation[]> {
    const response = await http.get<Conversation[]>(
        '/conversations',
        { signal },
    )

    return response.data
}

export async function createConversationAPI(
    title: string,
    signal?: AbortSignal,
): Promise<Conversation> {
    const response = await http.post<Conversation>(
        '/conversations',
        { title },
        { signal },
    )

    return response.data
}