import { describe, expect, it } from 'vitest'
import {
    applyAgentError,
    applyAgentEvent,
    applyAgentRecovery,
    beginConfirmation,
    blocksNewMessage,
    canConfirm,
    canRetryConfirmation,
    createAgentState,
    interruptAgentStream,
} from './agent-state'
import {
    decodeAgentRecovery,
    decodeAgentResult,
    decodeToolEvent,
} from '../api/agent-protocol'
import {
    agentResult, otherRunID, recovery, runID, runSummary, toolEvent,
} from '../test/agent-fixtures'
import { readyAgentState } from '../test/agent-ui-fixtures'

describe('Agent state lifecycle', () => {
    it('retries with original confirmation', () => {
        const proposalState = readyAgentState()
        const first = beginConfirmation(proposalState)
        expect(first).not.toBeNull()
        expect(beginConfirmation(proposalState)).toBeNull()

        const confirmationState = createAgentState(proposalState.operation)
        applyAgentEvent(confirmationState, {
            event: 'agent_start', data: { run_id: otherRunID },
        })
        interruptAgentStream(confirmationState)

        expect(confirmationState.operation?.phase).toBe('uncertain')
        expect(canRetryConfirmation(confirmationState)).toBe(false)
        expect(blocksNewMessage(confirmationState)).toBe(true)

        applyAgentRecovery(confirmationState, decodeAgentRecovery(recovery({
            stored: runSummary({
                run_id: otherRunID,
                decision: 'failed',
                message_save_status: 'not_attempted',
            }),
            message_save_status: 'not_attempted',
            messages: undefined,
        })), true)

        expect(canRetryConfirmation(confirmationState)).toBe(true)
        const retried = beginConfirmation(confirmationState)
        expect(retried).toEqual(first)
        expect(retried).not.toBe(first)
        expect(retried?.idempotency_key).toBe(
            proposalState.operation?.proposal.confirmed_action.idempotency_key,
        )
        expect(beginConfirmation(confirmationState)).toBeNull()
    })

    it('retains committed and saved facts', () => {
        const state = readyAgentState()
        applyAgentError(state, {
            code: 'agent_unavailable',
            message: 'fixture error',
            agent: decodeAgentResult(agentResult({
                execution_status: 'failed',
                business_effect: 'committed',
                message_save_status: 'unknown',
            })),
        })
        interruptAgentStream(state)

        applyAgentRecovery(state, decodeAgentRecovery(recovery({
            business_effect: 'none',
            message_save_status: 'unknown',
            messages: undefined,
        })), true)

        expect(state.businessEffect).toBe('committed')
        expect(state.messageSaveStatus).toBe('saved')
        expect(state.recoveryResolved).toBe(false)
        expect(state.interrupted).toBe(true)
        expect(canConfirm(state)).toBe(false)
        expect(blocksNewMessage(state)).toBe(true)
    })

    it('rejects conflicting tool identity', () => {
        const state = createAgentState()
        applyAgentEvent(state, {
            event: 'agent_start', data: { run_id: runID },
        })
        applyAgentEvent(state, {
            event: 'tool_start', data: decodeToolEvent(toolEvent(false), false),
        })

        expect(() => applyAgentEvent(state, {
            event: 'tool_end',
            data: decodeToolEvent({
                ...toolEvent(true), tool_name: 'get_rule_detail',
            }, true),
        })).toThrow()

        expect(() => applyAgentEvent(state, {
            event: 'tool_start',
            data: decodeToolEvent({
                ...toolEvent(false), call_id: otherRunID,
            }, false),
        })).toThrow()

        applyAgentEvent(state, {
            event: 'tool_end', data: decodeToolEvent(toolEvent(true), true),
        })
        expect(state.tools).toHaveLength(1)
        expect(state.tools[0]?.status).toBe('succeeded')

        expect(() => applyAgentEvent(state, {
            event: 'agent_start', data: { run_id: otherRunID },
        })).toThrow()
    })

    it('blocks partial recovery and expired confirmation', () => {
        const state = readyAgentState()
        expect(canConfirm(state)).toBe(true)
        expect(canConfirm(
            state,
            Date.parse(state.operation!.proposal.expires_at),
        )).toBe(false)

        interruptAgentStream(state)
        applyAgentRecovery(state, decodeAgentRecovery(recovery()), false)
        expect(state.recoveryResolved).toBe(false)
        expect(canConfirm(state)).toBe(false)
        expect(blocksNewMessage(state)).toBe(true)
    })
})