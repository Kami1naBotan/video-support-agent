const TOKEN_KEY = 'bili_support_token'

export function readToken(): string {
    return sessionStorage.getItem(TOKEN_KEY) ?? ''
}

export function saveToken(token: string): void {
    sessionStorage.setItem(TOKEN_KEY, token)
}

export function removeToken(): void {
    sessionStorage.removeItem(TOKEN_KEY)
}