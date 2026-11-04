import {
    decodeRuleCitations,
    isDecimalID,
    isRecord,
} from './rule-citation'

export function objectValue(value: unknown): Record<string, unknown> {
    if (!isRecord(value)) {
        throw new Error('协议数据必须是对象')
    }
    return value
}

export function stringValue(value: unknown): string {
    if (typeof value !== 'string') {
        throw new Error('协议字段必须是字符串')
    }
    return value
}

export function nonemptyString(value: unknown): string {
    const result = stringValue(value)
    if (result.trim() === '') {
        throw new Error('协议字段不能为空')
    }
    return result
}

export function booleanValue(value: unknown): boolean {
    if (typeof value !== 'boolean') {
        throw new Error('协议字段必须是布尔值')
    }
    return value
}

export function integerValue(
    value: unknown,
    minimum: number,
    maximum = Number.MAX_SAFE_INTEGER,
): number {
    if (
        typeof value !== 'number' ||
        !Number.isSafeInteger(value) ||
        value < minimum ||
        value > maximum
    ) {
        throw new Error('协议整数超出有效范围')
    }
    return value
}

export function positiveID(value: unknown): number {
    return integerValue(value, 1)
}

export function uuidValue(value: unknown): string {
    const result = stringValue(value)
    if (
        !/^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/.test(result)
    ) {
        throw new Error('Run、Call 或操作 Key 格式无效')
    }
    return result
}

function decimalID(value: unknown): string {
    if (!isDecimalID(value)) {
        throw new Error('工单 ID 格式无效')
    }
    return value
}

function timestamp(value: unknown): string {
    const result = stringValue(value)
    if (
        !/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,9})?(?:Z|[+-]\d{2}:\d{2})$/.test(result) ||
        !Number.isFinite(Date.parse(result))
    ) {
        throw new Error('协议时间格式无效')
    }
    return result
}

function choice<const T extends readonly string[]>(
    value: unknown,
    values: T,
): T[number] {
    for (const candidate of values) {
        if (candidate === value) return candidate
    }
    throw new Error('协议枚举值无效')
}

function optional<T>(
    value: unknown,
    decode: (value: unknown) => T,
): T | undefined {
    return value === undefined ? undefined : decode(value)
}

const executionStatuses = [
    'running', 'completed', 'failed', 'cancelled', 'timed_out', 'unknown',
] as const

const decisions = [
    'answer', 'clarify', 'handoff', 'confirmation_required',
    'ticket_created', 'ticket_status', 'failed',
] as const

const effects = ['none', 'committed', 'unknown'] as const

const saveStatuses = [
    'not_attempted', 'saved', 'failed', 'unknown',
] as const

const toolStatuses = [
    'started', 'succeeded', 'rejected', 'failed',
    'cancelled', 'timed_out', 'unknown',
] as const

const toolNames = [
    'search_rule',
    'get_rule_detail',
    'check_appeal_eligibility',
    'create_appeal_ticket',
    'query_ticket_status',
] as const

const auditCodes = [
    'none', 'invalid_argument', 'unauthorized', 'not_found',
    'configuration_error', 'invalid_result', 'result_too_large',
    'unavailable', 'timed_out', 'cancelled', 'outcome_unknown',
    'audit_unavailable', 'create_already_attempted',
    'idempotency_conflict', 'confirmation_required',
    'precondition_changed', 'needs_more_info', 'ineligible',
    'no_evidence', 'manual_review', 'protocol_error',
    'budget_exhausted', 'repeated_call', 'unsupported',
    'event_failed', 'model_failed',
] as const

const ticketStatuses = ['submitted', 'pending_review'] as const

export function decodeMessageReceipt(value: unknown) {
    const data = objectValue(value)
    const result = {
        user_message_id: positiveID(data.user_message_id),
        assistant_message_id: positiveID(data.assistant_message_id),
    }
    if (result.user_message_id === result.assistant_message_id) {
        throw new Error('两条消息不能使用相同 ID')
    }
    return result
}

function decodeCreatedTicket(value: unknown) {
    const data = objectValue(value)
    return {
        ticket_id: decimalID(data.ticket_id),
        initial_status: choice(data.initial_status, ticketStatuses),
        created_at: timestamp(data.created_at),
        replayed: booleanValue(data.replayed),
    }
}

function decodeTicketStatus(value: unknown) {
    const data = objectValue(value)
    return {
        ticket_id: decimalID(data.ticket_id),
        status: choice(data.status, ticketStatuses),
        created_at: timestamp(data.created_at),
        updated_at: timestamp(data.updated_at),
    }
}

export function decodeToolSummary(value: unknown) {
    const data = objectValue(value)
    return {
        call_id: uuidValue(data.call_id),
        sequence: integerValue(data.sequence, 1, 5),
        tool_name: choice(data.tool_name, toolNames),
        status: choice(data.status, toolStatuses),
        error_code: choice(data.error_code, auditCodes),
        business_effect: choice(data.business_effect, effects),
        audit_pending: booleanValue(data.audit_pending),
    }
}

function decodeTools(value: unknown) {
    if (!Array.isArray(value) || value.length > 5) {
        throw new Error('工具摘要列表无效')
    }

    const result = value.map(decodeToolSummary)
    const calls = new Set(result.map((item) => item.call_id))
    const sequences = new Set(result.map((item) => item.sequence))

    if (calls.size !== result.length || sequences.size !== result.length) {
        throw new Error('工具摘要存在重复 Call 或序号')
    }
    return result
}

export function decodeAgentRunSummary(value: unknown) {
    const data = objectValue(value)
    const executionStatus = choice(data.execution_status, executionStatuses)

    // StartRun 的持久化初值是 running + 空 decision。
    const decision = data.decision === '' && executionStatus === 'running'
        ? ''
        : choice(data.decision, decisions)

    return {
        run_id: uuidValue(data.run_id),
        execution_status: executionStatus,
        decision,
        reason_code: choice(data.reason_code, auditCodes),
        business_effect: choice(data.business_effect, effects),
        message_save_status: choice(data.message_save_status, saveStatuses),
        started_at: timestamp(data.started_at),
        finished_at: optional(data.finished_at, timestamp),
    }
}

export function decodeAgentResult(value: unknown) {
    const data = objectValue(value)
    return {
        run_id: optional(data.run_id, uuidValue),
        execution_status: choice(data.execution_status, executionStatuses),
        decision: choice(data.decision, decisions),
        reason_code: choice(data.reason_code, auditCodes),
        business_effect: choice(data.business_effect, effects),
        message_save_status: choice(data.message_save_status, saveStatuses),
        audit_pending: booleanValue(data.audit_pending),
        content: stringValue(data.content),
        ticket: optional(data.ticket, decodeCreatedTicket),
        ticket_status: optional(data.ticket_status, decodeTicketStatus),
        citations: decodeRuleCitations(data.citations),
        tools: decodeTools(data.tools),
    }
}

export function decodeAgentRecovery(value: unknown) {
    const data = objectValue(value)
    return {
        stored: decodeAgentRunSummary(data.stored),
        business_effect: choice(data.business_effect, effects),
        recovery_code: choice(data.recovery_code, [
            'none', 'outcome_unknown', 'idempotency_conflict',
        ]),
        audit_pending: booleanValue(data.audit_pending),
        message_save_status: choice(data.message_save_status, saveStatuses),
        messages: optional(data.messages, decodeMessageReceipt),
        ticket: optional(data.ticket, decodeCreatedTicket),
        tools: decodeTools(data.tools),
    }
}

export function decodeConfirmedAction(value: unknown) {
    const data = objectValue(value)
    const allowed = [
        'subject_type', 'subject_ref', 'reason', 'intake_mode',
        'idempotency_key', 'policy_digest', 'confirmation_token',
    ]

    if (
        Object.keys(data).length !== allowed.length ||
        Object.keys(data).some((key) => !allowed.includes(key))
    ) {
        throw new Error('确认请求字段不完整或包含未知字段')
    }

    const digest = stringValue(data.policy_digest)
    if (!/^[0-9a-f]{64}$/.test(digest)) {
        throw new Error('确认策略摘要格式无效')
    }

    return {
        subject_type: choice(data.subject_type, ['account', 'content', 'other']),
        subject_ref: nonemptyString(data.subject_ref),
        reason: nonemptyString(data.reason),
        intake_mode: choice(data.intake_mode, ['standard', 'manual_review']),
        idempotency_key: uuidValue(data.idempotency_key),
        policy_digest: digest,
        confirmation_token: nonemptyString(data.confirmation_token),
    }
}

function eligibilityReasonCode(value: unknown): string {
    const result = stringValue(value)
    if (
        result !== result.trim() ||
        !/^[a-z][a-z0-9_]{0,63}$/.test(result)
    ) {
        throw new Error('申诉资格原因码格式无效')
    }
    return result
}

export function decodeActionRequired(value: unknown) {
    const data = objectValue(value)
    const result = {
        run_id: uuidValue(data.run_id),
        confirmed_action: decodeConfirmedAction(data.confirmed_action),
        eligibility: choice(data.eligibility, [
            'eligible', 'ineligible', 'needs_more_info', 'manual_review',
        ]),
        reason_code: eligibilityReasonCode(data.reason_code),
        initial_status: choice(data.initial_status, ticketStatuses),
        issued_at: timestamp(data.issued_at),
        expires_at: timestamp(data.expires_at),
    }

    if (Date.parse(result.expires_at) <= Date.parse(result.issued_at)) {
        throw new Error('确认凭据有效期无效')
    }
    return result
}

export type AgentResult = ReturnType<typeof decodeAgentResult>
export type AgentRunSummary = ReturnType<typeof decodeAgentRunSummary>
export type AgentRecovery = ReturnType<typeof decodeAgentRecovery>
export type MessageReceipt = ReturnType<typeof decodeMessageReceipt>
export type ConfirmedAction = ReturnType<typeof decodeConfirmedAction>
export type ActionRequired = ReturnType<typeof decodeActionRequired>

export type AgentMessageRequest =
    | { content: string; confirmed_action?: never }
    | { confirmed_action: ConfirmedAction; content?: never }

export function encodeAgentRequest(
    value: AgentMessageRequest,
): AgentMessageRequest {
    const data = objectValue(value)
    const keys = Object.keys(data)

    if (keys.length !== 1) {
        throw new Error('只能提交文本或确认请求中的一种')
    }

    if (keys[0] === 'content') {
        return { content: nonemptyString(data.content) }
    }
    if (keys[0] === 'confirmed_action') {
        return { confirmed_action: decodeConfirmedAction(data.confirmed_action) }
    }
    throw new Error('消息请求字段无效')
}

export function decodeToolEvent(value: unknown, ending: boolean) {
    const data = objectValue(value)
    const result = {
        run_id: uuidValue(data.run_id),
        call_id: uuidValue(data.call_id),
        sequence: integerValue(data.sequence, 1, 5),
        tool_name: choice(data.tool_name, toolNames),
        status: choice(data.status, toolStatuses),
        error_code: optional(data.error_code, (v) => choice(v, auditCodes)),
        business_effect: choice(data.business_effect, effects),
        audit_pending: booleanValue(data.audit_pending),
        ticket_id: optional(data.ticket_id, decimalID),
        replayed: booleanValue(data.replayed),
        duration_ms: optional(data.duration_ms, (v) => integerValue(v, 0)),
    }

    if (ending) {
        if (
            result.status === 'started' ||
            result.error_code === undefined ||
            result.duration_ms === undefined
        ) {
            throw new Error('tool_end 缺少终态、错误码或耗时')
        }
        if (
            (result.replayed || result.business_effect === 'committed') &&
            result.ticket_id === undefined
        ) {
            throw new Error('工具已提交结果缺少工单 ID')
        }
    } else if (
        result.status !== 'started' ||
        result.business_effect !== 'none' ||
        result.error_code !== undefined ||
        result.duration_ms !== undefined ||
        result.ticket_id !== undefined ||
        result.replayed
    ) {
        throw new Error('tool_start 字段不符合开始事件协议')
    }

    return result
}

export type ToolEventData = ReturnType<typeof decodeToolEvent>

export interface AgentErrorData {
    code: string
    message: string
    run_id?: string
    agent?: AgentResult
    messages?: MessageReceipt
    recovery?: AgentRecovery
}

export function decodeAgentError(value: unknown): AgentErrorData {
    const data = objectValue(value)
    const result: AgentErrorData = {
        code: nonemptyString(data.code),
        message: nonemptyString(data.message),
    }

    if (data.run_id !== undefined) result.run_id = uuidValue(data.run_id)
    if (data.agent !== undefined) result.agent = decodeAgentResult(data.agent)
    if (data.messages !== undefined) {
        result.messages = decodeMessageReceipt(data.messages)
    }
    if (data.recovery !== undefined) {
        result.recovery = decodeAgentRecovery(data.recovery)
    }

    if (
        result.run_id !== undefined &&
        result.agent?.run_id !== undefined &&
        result.run_id !== result.agent.run_id
    ) {
        throw new Error('错误响应中的 Run 不一致')
    }
    return result
}

export type AgentDoneData = MessageReceipt & {
    run_id: string
    message_save_status: 'saved'
    agent: AgentResult
}

export function decodeAgentDone(value: unknown): AgentDoneData {
    const data = objectValue(value)
    const receipt = decodeMessageReceipt(data)
    const runID = uuidValue(data.run_id)
    const agent = decodeAgentResult(data.agent)

    if (
        data.message_save_status !== 'saved' ||
        agent.message_save_status !== 'saved' ||
        agent.run_id !== runID
    ) {
        throw new Error('done 未确认同一 Run 的消息保存结果')
    }

    return {
        ...receipt,
        run_id: runID,
        message_save_status: 'saved',
        agent,
    }
}