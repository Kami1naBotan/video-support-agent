import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createPinia, setActivePinia } from 'pinia'
import axios from 'axios'

import { useAuthStore } from './auth'
import { getCurrentUserAPI, loginAPI } from '../api/auth'
import type { CurrentUser, LoginResponse } from '../api/auth'

vi.mock('../api/auth', () => ({
    loginAPI: vi.fn(),
    getCurrentUserAPI: vi.fn(),
}))

function deferred<T>() {
    let resolve!: (value: T) => void

    const promise = new Promise<T>((resolvePromise) => {
        resolve = resolvePromise
    })

    return { promise, resolve }
}

describe('authentication lifecycle', () => {
    beforeEach(() => {
        sessionStorage.clear()
        vi.resetAllMocks()
        setActivePinia(createPinia())
    })

    it('stores token and loads the authenticated user', async () => {
        vi.mocked(loginAPI).mockResolvedValue({
            access_token: 'token-a',
            token_type: 'Bearer',
        })

        vi.mocked(getCurrentUserAPI).mockResolvedValue({
            id: 1,
            username: 'alice',
            role: 'user',
        })

        const auth = useAuthStore()

        await auth.login({
            username: 'alice',
            password: 'test-password',
        })

        expect(auth.user?.username).toBe('alice')
        expect(auth.token).toBe('token-a')
        expect(sessionStorage.getItem('bili_support_token')).toBe('token-a')
    })

    it('does not restore a login response that arrives after logout', async () => {
        const pending = deferred<LoginResponse>()
        vi.mocked(loginAPI).mockReturnValue(pending.promise)

        const auth = useAuthStore()

        const result = auth.login({
            username: 'alice',
            password: 'test-password',
        }).catch((error: unknown) => error)

        auth.logout()

        pending.resolve({
            access_token: 'old-token',
            token_type: 'Bearer',
        })

        expect(axios.isCancel(await result)).toBe(true)
        expect(auth.token).toBe('')
        expect(auth.user).toBeNull()
        expect(getCurrentUserAPI).not.toHaveBeenCalled()
    })

    it('does not let an old revision clear a new login', async () => {
        const auth = useAuthStore()
        const oldRevision = auth.revision

        vi.mocked(loginAPI).mockResolvedValue({
            access_token: 'new-token',
            token_type: 'Bearer',
        })

        vi.mocked(getCurrentUserAPI).mockResolvedValue({
            id: 2,
            username: 'bob',
            role: 'user',
        })

        await auth.login({
            username: 'bob',
            password: 'test-password',
        })

        expect(auth.invalidateIfCurrent(oldRevision)).toBe(false)
        expect(auth.user?.username).toBe('bob')
        expect(auth.token).toBe('new-token')
    })

    it('does not restore user data after logout', async () => {
        sessionStorage.setItem('bili_support_token', 'saved-token')

        const pending = deferred<CurrentUser>()
        vi.mocked(getCurrentUserAPI).mockReturnValue(pending.promise)

        const auth = useAuthStore()
        const result = auth.restoreSession().catch((error: unknown) => error)

        auth.logout()

        pending.resolve({
            id: 1,
            username: 'alice',
            role: 'user',
        })

        expect(axios.isCancel(await result)).toBe(true)
        expect(auth.user).toBeNull()
        expect(auth.token).toBe('')
    })

    it('preserves the token on a temporary network failure', async () => {
        sessionStorage.setItem('bili_support_token', 'saved-token')
        vi.mocked(getCurrentUserAPI).mockRejectedValue(new Error('network unavailable'))

        const auth = useAuthStore()

        await expect(auth.restoreSession()).rejects.toThrow('network unavailable')
        expect(auth.token).toBe('saved-token')
        expect(auth.user).toBeNull()
    })
})