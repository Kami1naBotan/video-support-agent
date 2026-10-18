import { http } from './http'

export interface LoginRequest {
    username: string
    password: string
}

export interface LoginResponse {
    access_token: string
    token_type: string
}

export interface CurrentUser {
    id: number
    username: string
    role: string
}

export async function loginAPI(
    input: LoginRequest,
    signal?: AbortSignal,
): Promise<LoginResponse> {
    const response = await http.post<LoginResponse>(
        '/auth/login',
        input,
        { signal },
    )

    return response.data
}

export async function getCurrentUserAPI(
    signal?: AbortSignal,
): Promise<CurrentUser> {
    const response = await http.get<CurrentUser>('/me', { signal })
    return response.data
}