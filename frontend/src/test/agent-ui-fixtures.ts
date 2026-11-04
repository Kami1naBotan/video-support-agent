import type { ChatMessage } from '../api/message'
import {
    decodeActionRequired,
    decodeAgentDone,
    decodeAgentRecovery,
    decodeAgentRunSummary,
} from '../api/agent-protocol'
import {
    applyAgentEvent,
    createAgentState,
    finishAgentStream,
} from '../chat/agent-state'
import {
    actionRequired,
    agentDone,
    agentResult,
    recovery,
    runID,
    runSummary,
} from './agent-fixtures'

export function uiProposal() {
    const now = Date.now()
    return decodeActionRequired({
        ...actionRequired(),
        issued_at: new Date(now).toISOString(),
        expires_at: new Date(now + 600_000).toISOString(),
    })
}

export function uiDone(confirmation = false) {
    return decodeAgentDone({
        ...agentDone(),
        agent: agentResult({
            decision: confirmation ? 'confirmation_required' : 'answer',
            message_save_status: 'saved',
        }),
    })
}

export function uiRecovery(confirmation = false) {
    return decodeAgentRecovery(recovery({
        stored: runSummary({
            decision: confirmation ? 'confirmation_required' : 'answer',
        }),
    }))
}

export function withAgentHistory(messages: ChatMessage[]): ChatMessage[] {
    return messages.map((message) => ({
        ...message,
        agent_run_id: runID,
        agent: decodeAgentRunSummary(runSummary()),
    }))
}

export function readyAgentState() {
    const state = createAgentState()
    applyAgentEvent(state, {
        event: 'agent_start',
        data: { run_id: runID },
    })
    applyAgentEvent(state, {
        event: 'action_required',
        data: uiProposal(),
    })
    applyAgentEvent(state, {
        event: 'done',
        data: uiDone(true),
    })
    finishAgentStream(state)
    return state
}