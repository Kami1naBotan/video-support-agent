import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import {
    clearRecoveryCheckpoints,
    readRecoveryCheckpoint,
    writeRecoveryCheckpoint,
} from './recovery-checkpoint'
import { otherRunID, runID } from '../test/agent-fixtures.ts'

const key = 'agent-recovery:v1:7:1'

beforeEach(() => {
    sessionStorage.clear()
})

afterEach(() => {
    vi.restoreAllMocks()
    sessionStorage.clear()
})

describe('Recovery checkpoint contract', () => {
    it('stores only scoped identifiers', () => {
        writeRecoveryCheckpoint(7, 1, runID, otherRunID)

        expect(JSON.parse(sessionStorage.getItem(key)!)).toEqual({
            version: 1,
            user_id: 7,
            conversation_id: 1,
            run_id: runID,
            operation_key: otherRunID,
        })
        expect(readRecoveryCheckpoint(7, 1)?.operation_key).toBe(otherRunID)
        expect(readRecoveryCheckpoint(8, 1)).toBeUndefined()
        expect(readRecoveryCheckpoint(7, 2)).toBeUndefined()
        expect(sessionStorage.getItem(key)).not.toMatch(
            /confirmation_token|subject_ref|policy_digest|reason/,
        )
    })

    it('rejects corrupt or foreign records', () => {
        const base = {
            version: 1, user_id: 7, conversation_id: 1, run_id: runID,
        }
        const invalid: unknown[] = [
            null, [], {},
            { ...base, user_id: 8 },
            { ...base, conversation_id: 2 },
            { ...base, version: 2 },
            { ...base, run_id: 'invalid' },
            { ...base, operation_key: 'invalid' },
            { ...base, confirmation_token: 'forbidden' },
        ]
        for (const value of invalid) {
            sessionStorage.setItem(key, JSON.stringify(value))
            expect(() => readRecoveryCheckpoint(7, 1)).toThrow()
        }
        sessionStorage.setItem(key, '{')
        expect(() => readRecoveryCheckpoint(7, 1)).toThrow()
    })

    it('fails closed on storage errors', () => {
        const set = vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
            throw new Error('fixture quota')
        })
        expect(() => writeRecoveryCheckpoint(7, 1, runID)).toThrow('不能安全继续')
        set.mockRestore()

        const get = vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
            throw new Error('fixture storage denied')
        })
        expect(() => readRecoveryCheckpoint(7, 1)).toThrow('核对已有执行结果')
        get.mockRestore()

        expect(() => writeRecoveryCheckpoint(0, 1, runID)).toThrow()
        expect(() => writeRecoveryCheckpoint(7, 1, 'invalid')).toThrow()
    })

    it('clears only recovery keys', () => {
        writeRecoveryCheckpoint(7, 1, runID)
        writeRecoveryCheckpoint(8, 2, otherRunID)
        sessionStorage.setItem('unrelated-preference', 'keep')

        clearRecoveryCheckpoints()

        expect(readRecoveryCheckpoint(7, 1)).toBeUndefined()
        expect(readRecoveryCheckpoint(8, 2)).toBeUndefined()
        expect(sessionStorage.getItem('unrelated-preference')).toBe('keep')
    })
})