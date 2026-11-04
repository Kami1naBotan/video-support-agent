import type { ChatStreamEvent } from '../api/chat-stream'
import {
    decodeConfirmedAction,
    type ActionRequired,
    type AgentErrorData,
    type AgentResult,
    type ConfirmedAction,
    type MessageReceipt,
    type AgentRecovery,
} from '../api/agent-protocol'

type ToolSummary = AgentResult['tools'][number]

export type ToolRow = ToolSummary & {
    duration_ms?: number
    ticket_id?: string
    replayed?: boolean
}

export type ConfirmationPhase =
    | 'ready'
    | 'submitting'
    | 'uncertain'
    | 'submitted'
    | 'dismissed'

export interface ConfirmationOperation {
    proposal: ActionRequired
    phase: ConfirmationPhase
}

export interface AgentUIState {
    runID?: string
    tools: ToolRow[]
    result?: AgentResult
    receipt?: MessageReceipt
    operation?: ConfirmationOperation
    businessEffect: AgentResult['business_effect']
    messageSaveStatus: AgentResult['message_save_status']
    ticketID?: string
    busy: boolean
    completed: boolean
    interrupted: boolean
    recovery?: AgentRecovery
    recoveryResolved?: boolean
}

export function createAgentState(
    operation?: ConfirmationOperation,
): AgentUIState {
    return {
        tools: [],
        operation,
        businessEffect: 'none',
        messageSaveStatus: 'not_attempted',
        busy: true,
        completed: false,
        interrupted: false,
    }
}

function associateRun(state: AgentUIState, runID: string): void {
    if (state.runID !== undefined && state.runID !== runID) {
        throw new Error('执行状态关联了不同的 Run')
    }
    state.runID = runID
}

function retainBusinessEffect(
    state: AgentUIState,
    effect: AgentResult['business_effect'],
): void {
    // 本次 Run 已观察到提交事实，之后的错误不能抹掉它。
    if (state.businessEffect === 'committed') return

    if (effect === 'committed' || effect === 'unknown') {
        state.businessEffect = effect
    }
}

function mergeTool(
    state: AgentUIState,
    incoming: ToolRow,
    requireStart: boolean,
): void {
    const previous = state.tools.find(
        (item) => item.call_id === incoming.call_id,
    )

    if (!previous) {
        if (requireStart && incoming.status !== 'started') {
            throw new Error('工具结束事件缺少对应的开始事件')
        }

        if (
            state.tools.some((item) => item.sequence === incoming.sequence)
        ) {
            throw new Error('不同工具调用使用了同一序号')
        }

        state.tools.push({ ...incoming })
        state.tools.sort((a, b) => a.sequence - b.sequence)
        return
    }

    if (
        previous.sequence !== incoming.sequence ||
        previous.tool_name !== incoming.tool_name
    ) {
        throw new Error('同一 Call 的工具名称或序号发生变化')
    }

    if (previous.status !== 'started') {
        // 后来的摘要不能把已结束工具退回“执行中”。
        if (incoming.status === 'started') return

        if (previous.status !== incoming.status) {
            throw new Error('同一 Call 出现冲突的终态')
        }
    }

    const committed =
        previous.business_effect === 'committed' ||
        incoming.business_effect === 'committed'

    Object.assign(previous, incoming)

    if (committed) {
        previous.business_effect = 'committed'
    }
}

export function applyAgentResult(
    state: AgentUIState,
    result: AgentResult,
): void {
    if (result.run_id !== undefined) {
        associateRun(state, result.run_id)
    }

    // 先保留明确业务事实，再处理展示列表。
    retainBusinessEffect(state, result.business_effect)

    if (result.ticket !== undefined) {
        state.ticketID = result.ticket.ticket_id
    }

    if (state.messageSaveStatus !== 'saved') {
        state.messageSaveStatus = result.message_save_status
    }

    state.result = result

    for (const tool of result.tools) {
        mergeTool(state, tool, false)
    }

    if (
        state.operation?.phase === 'submitting' &&
        state.businessEffect === 'committed'
    ) {
        state.operation.phase = 'submitted'
    }
}

export function applyAgentError(
    state: AgentUIState,
    details: AgentErrorData,
): void {
    if (details.run_id !== undefined) {
        associateRun(state, details.run_id)
    }

    if (details.agent !== undefined) {
        applyAgentResult(state, details.agent)
    }

    if (details.messages !== undefined) {
        state.receipt = details.messages
        state.messageSaveStatus = 'saved'
    }
}

export function applyAgentEvent(
    state: AgentUIState,
    event: ChatStreamEvent,
): void {
    switch (event.event) {
        case 'agent_start':
            associateRun(state, event.data.run_id)
            return

        case 'tool_start':
        case 'tool_end': {
            const data = event.data
            associateRun(state, data.run_id)
            retainBusinessEffect(state, data.business_effect)

            if (data.ticket_id !== undefined) {
                state.ticketID = data.ticket_id
            }

            mergeTool(
                state,
                {
                    ...data,
                    error_code: data.error_code ?? 'none',
                },
                true,
            )

            if (
                state.operation?.phase === 'submitting' &&
                state.businessEffect === 'committed'
            ) {
                state.operation.phase = 'submitted'
            }
            return
        }

        case 'action_required':
            associateRun(state, event.data.run_id)

            if (state.operation !== undefined) {
                throw new Error('同一轮出现重复或冲突的确认卡片')
            }

            state.operation = {
                proposal: event.data,
                phase: 'ready',
            }
            return

        case 'agent_result':
            applyAgentResult(state, event.data)
            return

        case 'done':
            if ('agent' in event.data) {
                associateRun(state, event.data.run_id)
                applyAgentResult(state, event.data.agent)
            }

            state.receipt = {
                user_message_id: event.data.user_message_id,
                assistant_message_id: event.data.assistant_message_id,
            }
            state.messageSaveStatus = 'saved'
            state.completed = true
            return

        case 'error':
            applyAgentError(state, event.data)
            return

        case 'start':
        case 'token':
        case 'citation':
            return
    }
}

export function finishAgentStream(state: AgentUIState): void {
    state.busy = false

    if (state.operation?.phase === 'submitting') {
        state.operation.phase =
            state.businessEffect === 'committed'
                ? 'submitted'
                : 'uncertain'
    }
}

export function interruptAgentStream(state: AgentUIState): void {
    state.busy = false
    state.interrupted = true

    if (state.operation?.phase === 'submitting') {
        state.operation.phase =
            state.businessEffect === 'committed'
                ? 'submitted'
                : 'uncertain'
    }
}

export function canConfirm(
    state: AgentUIState,
    now = Date.now(),
): boolean {
    const operation = state.operation

    if (
        operation === undefined ||
        state.busy ||
        state.interrupted ||
        state.businessEffect !== 'none'
    ) {
        return false
    }

    const retry = canRetryConfirmation(state)
    const decision =
        state.result?.decision ??
        state.recovery?.stored.decision

    if (!retry) {
        if (
            operation.phase !== 'ready' ||
            !state.completed ||
            state.messageSaveStatus !== 'saved' ||
            decision !== 'confirmation_required' ||
            operation.proposal.run_id !== state.runID
        ) {
            return false
        }
    }

    const proposal = operation.proposal
    const input = proposal.confirmed_action

    if (Date.parse(proposal.expires_at) <= now) {
        return false
    }

    return (
        (
            proposal.eligibility === 'eligible' &&
            input.intake_mode === 'standard' &&
            proposal.initial_status === 'submitted'
        ) ||
        (
            proposal.eligibility === 'manual_review' &&
            input.intake_mode === 'manual_review' &&
            proposal.initial_status === 'pending_review'
        )
    )
}

export function beginConfirmation(
    state: AgentUIState,
): ConfirmedAction | null {
    if (!canConfirm(state)) return null

    const operation = state.operation
    if (operation === undefined) return null

    // 解码得到独立副本，保留服务端给出的全部原字段。
    const request = decodeConfirmedAction(
        operation.proposal.confirmed_action,
    )

    // 在任何 await 之前设置，连续点击也只能通过一次。
    operation.phase = 'submitting'
    return request
}

export function dismissConfirmation(state: AgentUIState): void {
    if (
        !state.busy &&
        (
            state.operation?.phase === 'ready' ||
            canRetryConfirmation(state)
        )
    ) {
        state.operation = undefined
    }
}

export function blocksNewMessage(state: AgentUIState): boolean {
    const phase = state.operation?.phase

    return (
        state.busy ||
        state.interrupted ||
        state.businessEffect === 'unknown' ||
        phase === 'ready' ||
        phase === 'submitting' ||
        phase === 'uncertain'
    )
}

export function applyAgentRecovery(
    state: AgentUIState,
    recovery: AgentRecovery,
    completeResponse: boolean,
): void {
    associateRun(state, recovery.stored.run_id)

    state.recovery = recovery
    state.recoveryResolved = false
    state.busy = false

    // 恢复查询可能读到比已收到的流式事件更旧的审计状态。
    // 已知 committed 和 saved 不向后退。
    if (state.businessEffect !== 'committed') {
        if (
            recovery.business_effect === 'committed' ||
            recovery.business_effect === 'unknown'
        ) {
            state.businessEffect = recovery.business_effect
        } else if (completeResponse) {
            state.businessEffect = 'none'
        }
    }

    if (recovery.ticket !== undefined) {
        state.ticketID = recovery.ticket.ticket_id
    }

    if (state.messageSaveStatus !== 'saved') {
        state.messageSaveStatus = recovery.message_save_status
    }

    if (recovery.messages !== undefined) {
        state.receipt = recovery.messages
        state.messageSaveStatus = 'saved'
    }

    for (const tool of recovery.tools) {
        const previous = state.tools.find(
            (item) => item.call_id === tool.call_id,
        )

        if (
            previous !== undefined &&
            previous.status !== 'started' &&
            previous.status !== tool.status
        ) {
            // 保留已收到的终态，原始恢复观察另存于 recovery。
            continue
        }

        mergeTool(state, tool, false)
    }

    const executionSettled =
        recovery.stored.execution_status !== 'running' &&
        recovery.stored.execution_status !== 'unknown'

    const receiptConsistent =
        recovery.message_save_status !== 'saved' ||
        recovery.messages !== undefined

    const knownFactsConsistent =
        !(
            state.businessEffect === 'committed' &&
            recovery.business_effect !== 'committed'
        ) &&
        !(
            state.messageSaveStatus === 'saved' &&
            recovery.message_save_status !== 'saved'
        )

    state.recoveryResolved =
        completeResponse &&
        executionSettled &&
        !recovery.audit_pending &&
        recovery.recovery_code === 'none' &&
        recovery.business_effect !== 'unknown' &&
        recovery.message_save_status !== 'unknown' &&
        receiptConsistent &&
        knownFactsConsistent

    state.interrupted = !state.recoveryResolved
    state.completed =
        state.recoveryResolved &&
        state.messageSaveStatus === 'saved'

    const operation = state.operation
    if (
        operation !== undefined &&
        operation.phase !== 'ready' &&
        operation.phase !== 'dismissed'
    ) {
        operation.phase =
            state.businessEffect === 'committed'
                ? 'submitted'
                : 'uncertain'
    }
}

export function canRetryConfirmation(state: AgentUIState): boolean {
    return (
        state.operation?.phase === 'uncertain' &&
        state.recoveryResolved === true &&
        state.businessEffect === 'none' &&
        !state.busy &&
        !state.interrupted
    )
}