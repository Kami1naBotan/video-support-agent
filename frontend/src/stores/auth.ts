import { ref } from 'vue'
import { defineStore } from 'pinia'
import { CanceledError } from 'axios'

import { getCurrentUserAPI, loginAPI } from '../api/auth'
import type { CurrentUser, LoginRequest } from '../api/auth'
import { isUnauthorized } from '../api/errors'
import { readToken, removeToken, saveToken } from '../auth/token'

export const useAuthStore = defineStore('auth', () => {
    const token = ref(readToken())
    const user = ref<CurrentUser | null>(null)

    const revision = ref(0)

    let activeController: AbortController | null = null

    function cancelPending() {
        activeController?.abort()
        activeController = null
    }

    function logout() {
        cancelPending()
        revision.value++
        token.value = ''
        user.value = null
        removeToken()
    }

    function ensureCurrent(
        expectedRevision: number,
        controller: AbortController,
    ) {
        if (
            expectedRevision !== revision.value ||
            controller.signal.aborted
        ) {
            throw new CanceledError('Authentication operation is stale')
        }
    }

    function invalidateIfCurrent(expectedRevision: number): boolean {
        if (expectedRevision !== revision.value) {
            return false
        }

        logout()
        return true
    }

    async function login(input: LoginRequest) {
        logout()

        const expectedRevision = revision.value
        const controller = new AbortController()
        activeController = controller

        try {
            const result = await loginAPI(input, controller.signal)
            ensureCurrent(expectedRevision, controller)

            token.value = result.access_token
            saveToken(result.access_token)

            const currentUser = await getCurrentUserAPI(controller.signal)
            ensureCurrent(expectedRevision, controller)

            user.value = currentUser
        } catch (error) {
            const stale =
                expectedRevision !== revision.value ||
                controller.signal.aborted

            if (stale) {
                throw new CanceledError('Authentication operation is stale')
            }

            invalidateIfCurrent(expectedRevision)
            throw error
        } finally {
            if (activeController === controller) {
                activeController = null
            }
        }
    }

    async function restoreSession() {
        if (!token.value || user.value) {
            return
        }

        cancelPending()

        const expectedRevision = revision.value
        const controller = new AbortController()
        activeController = controller

        try {
            const currentUser = await getCurrentUserAPI(controller.signal)
            ensureCurrent(expectedRevision, controller)
            user.value = currentUser
        } catch (error) {
            if (
                expectedRevision !== revision.value ||
                controller.signal.aborted
            ) {
                throw new CanceledError('Session restoration is stale')
            }

            if (isUnauthorized(error)) {
                invalidateIfCurrent(expectedRevision)
                return
            }

            throw error
        } finally {
            if (activeController === controller) {
                activeController = null
            }
        }
    }

    return {
        token,
        user,
        revision,
        login,
        logout,
        restoreSession,
        invalidateIfCurrent,
    }
})