import { beforeEach, describe, expect, it, vi } from 'vitest'
import { http } from './http'
import {
    decodeActionRequired,
    decodeAgentDone,
    decodeAgentRecovery,
    decodeAgentResult,
    decodeAgentRunSummary,
    decodeConfirmedAction,
    decodeMessageReceipt,
    decodeToolEvent,
    encodeAgentRequest,
} from './agent-protocol'
import {
    decodeAgentMessageResponse,
    decodeChatMessage,
    listMessagesAPI,
    recoverAgentRunAPI,
    sendAgentMessageAPI,
} from './message'
import { decodeRequestError } from './chat-stream-error'
import {
    actionRequired, agentDone, agentResult, confirmedAction,
    otherRunID, receipt, recovery, runID, runSummary,
    savedResponse, toolEvent,
} from '../test/agent-fixtures'

vi.mock('./http', () => ({
    http: { get: vi.fn(), post: vi.fn() },
}))

beforeEach(() => {
    vi.resetAllMocks()
})

describe('Agent protocol contract', () => {
    it('accepts valid projections', () => {
        const input = agentResult()
        const result = decodeAgentResult(input)
        expect(result.run_id).toBe(runID)
        expect(result.content).toBe(input.content)
        expect(result).not.toBe(input)
        expect(result.tools).not.toBe(input.tools)
        expect(decodeAgentDone(agentDone())).not.toHaveProperty('messages')
        expect(decodeAgentDone(agentDone()).assistant_message_id).toBe(11)
        expect(decodeAgentRunSummary(runSummary({
            execution_status: 'running',
            decision: '',
        })).decision).toBe('')

        const largeTicket = decodeAgentResult(agentResult({
            ticket: {
                ticket_id: '18446744073709551615',
                initial_status: 'pending_review',
                created_at: '2026-11-04T08:00:00Z',
                replayed: true,
            },
        }))
        expect(largeTicket.ticket?.ticket_id).toBe('18446744073709551615')
    })

    it('rejects invalid result fields', () => {
        const invalid = [
            { run_id: 'invalid' },
            { execution_status: 'finished' },
            { decision: 'invented' },
            { reason_code: 'private_exception' },
            { business_effect: 'success' },
            { message_save_status: 'not_saved' },
            { audit_pending: 'false' },
            { content: null },
            { citations: null },
            { tools: null },
            { tools: [toolEvent(true), toolEvent(true)] },
        ]
        for (const patch of invalid) {
            expect(() => decodeAgentResult(agentResult(patch))).toThrow()
        }
        for (const value of [null, [], {}, 'text']) {
            expect(() => decodeAgentResult(value)).toThrow()
        }
        for (const value of [
            { user_message_id: 10, assistant_message_id: 10 },
            { user_message_id: 0, assistant_message_id: 11 },
            { user_message_id: Number.MAX_SAFE_INTEGER + 1, assistant_message_id: 11 },
        ]) {
            expect(() => decodeMessageReceipt(value)).toThrow()
        }
        expect(() => decodeAgentRunSummary(runSummary({ decision: '' }))).toThrow()
        expect(() => decodeAgentDone({
            ...agentDone(), run_id: otherRunID,
        })).toThrow()
        expect(() => decodeAgentDone({
            ...agentDone(), message_save_status: 'unknown',
        })).toThrow()
    })

    it('binds history and ordinary responses', () => {
        const history = { ...savedResponse(), agent: runSummary() }
        expect(decodeChatMessage(history).agent?.run_id).toBe(runID)
        expect(decodeAgentMessageResponse(savedResponse()).messages).toEqual(receipt())

        for (const value of [
            { ...history, agent: undefined },
            { ...history, agent_run_id: undefined },
            { ...history, agent: runSummary({ run_id: otherRunID }) },
        ]) {
            expect(() => decodeChatMessage(value)).toThrow()
        }
        for (const patch of [
            { role: 'assistant' },
            { agent_run_id: otherRunID },
            { messages: { user_message_id: 12, assistant_message_id: 11 } },
            { agent: agentResult({ message_save_status: 'unknown' }) },
            { agent: agentResult({ decision: 'confirmation_required', message_save_status: 'saved' }) },
            { action_required: actionRequired() },
        ]) {
            expect(() => decodeAgentMessageResponse({
                ...savedResponse(), ...patch,
            })).toThrow()
        }

        const proposal = decodeAgentMessageResponse({
            ...savedResponse(),
            agent: agentResult({
                decision: 'confirmation_required',
                message_save_status: 'saved',
            }),
            action_required: actionRequired(),
        })
        expect(proposal.action_required?.confirmed_action.idempotency_key).toBe(otherRunID)
    })

    it('validates confirmation and tool events', () => {
        const action = decodeConfirmedAction(confirmedAction())
        expect(encodeAgentRequest({ confirmed_action: action })).toEqual({
            confirmed_action: action,
        })
        expect(() => decodeConfirmedAction({
            ...confirmedAction(), user_id: 7,
        })).toThrow()
        expect(() => decodeConfirmedAction({
            ...confirmedAction(), policy_digest: 'A'.repeat(64),
        })).toThrow()
        expect(() => decodeConfirmedAction({
            ...confirmedAction(), confirmation_token: '',
        })).toThrow()
        expect(() => decodeActionRequired({
            ...actionRequired(), expires_at: actionRequired().issued_at,
        })).toThrow()

        expect(decodeToolEvent(toolEvent(false), false).status).toBe('started')
        expect(decodeToolEvent(toolEvent(true), true).status).toBe('succeeded')
        for (const patch of [
            { sequence: 0 }, { sequence: 6 }, { tool_name: 'shell' },
            { call_id: 'invalid' }, { duration_ms: -1 },
            { business_effect: 'committed' }, { replayed: true },
        ]) {
            expect(() => decodeToolEvent({
                ...toolEvent(true), ...patch,
            }, true)).toThrow()
        }
        expect(() => decodeToolEvent(toolEvent(true), false)).toThrow()
        expect(() => decodeToolEvent(toolEvent(false), true)).toThrow()
    })

    it('preserves recovery and rich errors', () => {
        const partial = recovery({
            stored: runSummary({
                execution_status: 'running',
                business_effect: 'unknown',
                message_save_status: 'unknown',
            }),
            business_effect: 'committed',
            audit_pending: true,
            message_save_status: 'unknown',
            messages: undefined,
        })
        const decoded = decodeAgentRecovery(partial)
        expect(decoded.stored.business_effect).toBe('unknown')
        expect(decoded.business_effect).toBe('committed')
        expect(decoded.messages).toBeUndefined()

        const error = decodeRequestError(503, {
            code: 'agent_audit_unavailable',
            message: '请查询恢复结果',
            recovery: partial,
        })
        expect(error.status).toBe(503)
        expect(error.details?.recovery?.business_effect).toBe('committed')

        const invalid = decodeRequestError(503, {
            code: 'error', message: 'failure',
            run_id: runID,
            agent: agentResult({ run_id: otherRunID }),
        })
        expect(invalid.code).toBe('invalid_response')
        expect(invalid.details).toBeUndefined()

        const projected = decodeAgentResult({
            ...agentResult(),
            confirmation_token: 'must-not-escape',
            raw_audit: { private: true },
        })
        expect(JSON.stringify(projected)).not.toContain('must-not-escape')
        expect(projected).not.toHaveProperty('raw_audit')
    })
})

function httpFailure(data: unknown) {
    return {
        isAxiosError: true,
        response: { status: 503, data },
    }
}

describe('Agent HTTP wiring', () => {
    it('sends confirmation and cancellation signal', async () => {
        const signal = new AbortController().signal
        const action = decodeConfirmedAction(confirmedAction())
        vi.mocked(http.post).mockResolvedValueOnce({ data: savedResponse() })

        const result = await sendAgentMessageAPI(1, { confirmed_action: action }, signal)
        expect(http.post).toHaveBeenCalledExactlyOnceWith(
            '/conversations/1/messages',
            { confirmed_action: action },
            { signal },
        )
        expect(result.messages).toEqual(receipt())
    })

    it('rejects foreign history and responses', async () => {
        vi.mocked(http.get).mockResolvedValueOnce({
            data: [{ ...savedResponse(), conversation_id: 2, agent: runSummary() }],
        })
        await expect(listMessagesAPI(1)).rejects.toThrow()

        vi.mocked(http.post).mockResolvedValueOnce({
            data: { ...savedResponse(), conversation_id: 2 },
        })
        await expect(sendAgentMessageAPI(1, { content: 'hello' })).rejects.toThrow()

        vi.mocked(http.post).mockClear()
        await expect(sendAgentMessageAPI(0, { content: 'hello' })).rejects.toThrow()
        expect(http.post).not.toHaveBeenCalled()
    })

    it('preserves HTTP failure facts', async () => {
        vi.mocked(http.post).mockRejectedValueOnce(httpFailure({
            code: 'agent_message_save_unconfirmed',
            message: '保存待确认',
            agent: agentResult({
                business_effect: 'committed',
                message_save_status: 'unknown',
            }),
        }))
        await expect(sendAgentMessageAPI(1, { content: 'hello' })).rejects.toMatchObject({
            status: 503,
            code: 'agent_message_save_unconfirmed',
            details: {
                agent: {
                    run_id: runID,
                    business_effect: 'committed',
                    message_save_status: 'unknown',
                },
            },
        })
    })

    it('validates recovery Run on success and failure', async () => {
        const signal = new AbortController().signal
        vi.mocked(http.get).mockResolvedValueOnce({ data: recovery() })
        expect((await recoverAgentRunAPI(1, runID, signal)).stored.run_id).toBe(runID)
        expect(http.get).toHaveBeenLastCalledWith(
            `/conversations/1/agent-runs/${runID}`, { signal },
        )

        const foreign = recovery({ stored: runSummary({ run_id: otherRunID }) })
        vi.mocked(http.get).mockResolvedValueOnce({ data: foreign })
        await expect(recoverAgentRunAPI(1, runID)).rejects.toThrow('Run')

        vi.mocked(http.get).mockRejectedValueOnce(httpFailure({
            code: 'agent_audit_unavailable', message: 'partial', recovery: foreign,
        }))
        await expect(recoverAgentRunAPI(1, runID)).rejects.toThrow('Run')

        vi.mocked(http.get).mockRejectedValueOnce(httpFailure({
            code: 'agent_audit_unavailable',
            message: 'partial',
            recovery: recovery({ business_effect: 'committed' }),
        }))
        await expect(recoverAgentRunAPI(1, runID)).rejects.toMatchObject({
            details: { recovery: { business_effect: 'committed' } },
        })
    })
})