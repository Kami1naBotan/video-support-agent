export const runID = '11111111-1111-4111-8111-111111111111'
export const otherRunID = '22222222-2222-4222-8222-222222222222'
export const callID = '33333333-3333-4333-8333-333333333333'
export const at = '2026-11-04T08:00:00Z'

export function confirmedAction() {
    return {
        subject_type: 'account',
        subject_ref: 'fixture-account',
        reason: 'fixture reason',
        intake_mode: 'manual_review',
        idempotency_key: otherRunID,
        policy_digest: 'a'.repeat(64),
        confirmation_token: 'fixture-only-token',
    }
}

export function actionRequired() {
    return {
        run_id: runID,
        confirmed_action: confirmedAction(),
        eligibility: 'manual_review',
        reason_code: 'manual_review',
        initial_status: 'pending_review',
        issued_at: at,
        expires_at: '2026-11-04T08:10:00Z',
    }
}

export function agentResult(overrides: Record<string, unknown> = {}) {
    return {
        run_id: runID,
        execution_status: 'completed',
        decision: 'answer',
        reason_code: 'none',
        business_effect: 'none',
        message_save_status: 'not_attempted',
        audit_pending: false,
        content: '你好，核对结果 🙂',
        citations: [],
        tools: [],
        ...overrides,
    }
}

export function runSummary(overrides: Record<string, unknown> = {}) {
    return {
        run_id: runID,
        execution_status: 'completed',
        decision: 'answer',
        reason_code: 'none',
        business_effect: 'none',
        message_save_status: 'saved',
        started_at: at,
        finished_at: at,
        ...overrides,
    }
}

export function receipt() {
    return { user_message_id: 10, assistant_message_id: 11 }
}

export function savedResponse() {
    return {
        id: 10,
        conversation_id: 1,
        role: 'user',
        content: 'fixture question',
        created_at: at,
        agent_run_id: runID,
        agent: agentResult({ message_save_status: 'saved' }),
        messages: receipt(),
    }
}

export function recovery(overrides: Record<string, unknown> = {}) {
    return {
        stored: runSummary(),
        business_effect: 'none',
        recovery_code: 'none',
        audit_pending: false,
        message_save_status: 'saved',
        messages: receipt(),
        tools: [],
        ...overrides,
    }
}

export function toolEvent(ending: boolean) {
    const base = {
        run_id: runID,
        call_id: callID,
        sequence: 1,
        tool_name: 'search_rule',
        status: ending ? 'succeeded' : 'started',
        business_effect: 'none',
        audit_pending: false,
        replayed: false,
    }
    return ending
        ? { ...base, error_code: 'none', duration_ms: 1 }
        : base
}

export function agentDone() {
    return {
        ...receipt(),
        run_id: runID,
        message_save_status: 'saved',
        agent: agentResult({ message_save_status: 'saved' }),
    }
}