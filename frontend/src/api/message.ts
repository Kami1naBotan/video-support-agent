import { http } from './http'
import { decodeRuleCitations, isRecord } from './rule-citation'
import type { RuleCitation } from './rule-citation'
import axios from 'axios'
import {
    decodeActionRequired,
    decodeAgentRecovery,
    decodeAgentResult,
    decodeAgentRunSummary,
    decodeMessageReceipt,
    encodeAgentRequest,
    objectValue,
    positiveID,
    uuidValue,
    type ActionRequired,
    type AgentMessageRequest,
    type AgentRecovery,
    type AgentResult,
    type AgentRunSummary,
    type MessageReceipt,
} from './agent-protocol'
import { decodeRequestError } from './chat-stream-error'

export interface ChatMessage {
    id: number
    conversation_id: number
    role: string
    content: string
    created_at: string
    citations?: RuleCitation[]
    agent_run_id?: string
    agent?: AgentRunSummary
}

function decodeMessageBase(
    value: unknown,
): Omit<ChatMessage, 'agent' | 'agent_run_id'> {
    if (
        !isRecord(value) ||
        typeof value.id !== 'number' ||
        !Number.isSafeInteger(value.id) ||
        value.id <= 0 ||
        typeof value.conversation_id !== 'number' ||
        !Number.isSafeInteger(value.conversation_id) ||
        value.conversation_id <= 0 ||
        typeof value.role !== 'string' ||
        value.role.trim() === '' ||
        typeof value.content !== 'string' ||
        typeof value.created_at !== 'string'
    ) {
        throw new Error('消息数据格式无效')
    }

    const citations =
        value.citations === undefined || value.citations === null
            ? []
            : decodeRuleCitations(value.citations)

    if (value.role !== 'assistant' && citations.length > 0) {
        throw new Error('非助手消息不能携带规则引用')
    }

    return {
        id: value.id,
        conversation_id: value.conversation_id,
        role: value.role,
        content: value.content,
        created_at: value.created_at,
        citations,
    }
}

export function decodeChatMessage(value: unknown): ChatMessage {
    const data = objectValue(value)
    const message: ChatMessage = decodeMessageBase(data)

    if (data.agent_run_id !== undefined) {
        message.agent_run_id = uuidValue(data.agent_run_id)
    }
    if (data.agent !== undefined) {
        message.agent = decodeAgentRunSummary(data.agent)
    }

    if (
        (message.agent_run_id === undefined) !==
        (message.agent === undefined)
    ) {
        throw new Error('历史消息缺少对应的 Run 摘要')
    }

    if (
        message.agent !== undefined &&
        message.agent.run_id !== message.agent_run_id
    ) {
        throw new Error('历史消息与 Run 摘要不匹配')
    }

    return message
}

export async function listMessagesAPI(
    conversationID: number,
    signal?: AbortSignal,
): Promise<ChatMessage[]> {
    positiveID(conversationID)
    const response = await http.get<unknown>(
        `/conversations/${conversationID}/messages`,
        { signal },
    )

    if (!Array.isArray(response.data)) {
        throw new Error('消息列表格式无效')
    }

    return response.data.map((value) => {
        const message = decodeChatMessage(value)
        if (message.conversation_id !== conversationID) {
            throw new Error('历史消息不属于当前会话')
        }
        return message
    })
}

export type AgentMessageResponse = Omit<ChatMessage, 'agent'> & {
    agent?: AgentResult
    messages?: MessageReceipt
    action_required?: ActionRequired
}

export function decodeAgentMessageResponse(
    value: unknown,
): AgentMessageResponse {
    const data = objectValue(value)
    const result: AgentMessageResponse = decodeMessageBase(data)

    if (data.agent_run_id !== undefined) {
        result.agent_run_id = uuidValue(data.agent_run_id)
    }

    if (data.agent === undefined) {
        if (
            data.messages !== undefined ||
            data.action_required !== undefined ||
            data.agent_run_id !== undefined
        ) {
            throw new Error('普通响应缺少 Agent 执行结果')
        }
        return result
    }

    result.agent = decodeAgentResult(data.agent)
    result.messages = decodeMessageReceipt(data.messages)

    if (
        result.role !== 'user' ||
        result.agent.run_id === undefined ||
        result.agent_run_id !== result.agent.run_id ||
        result.agent.message_save_status !== 'saved' ||
        result.messages.user_message_id !== result.id
    ) {
        throw new Error('普通响应的消息、Run 或保存回执不一致')
    }

    if (data.action_required !== undefined) {
        result.action_required = decodeActionRequired(data.action_required)
        if (
            result.action_required.run_id !== result.agent.run_id ||
            result.agent.decision !== 'confirmation_required' ||
            result.agent.business_effect !== 'none'
        ) {
            throw new Error('确认卡片与执行结果不一致')
        }
    }

    if (
        result.agent.decision === 'confirmation_required' &&
        result.action_required === undefined
    ) {
        throw new Error('确认结果缺少确认卡片')
    }

    return result
}

function rethrowRequestError(error: unknown): never {
    if (axios.isAxiosError(error) && error.response !== undefined) {
        throw decodeRequestError(error.response.status, error.response.data)
    }
    throw error
}

export async function sendAgentMessageAPI(
    conversationID: number,
    request: AgentMessageRequest,
    signal?: AbortSignal,
): Promise<AgentMessageResponse> {
    positiveID(conversationID)
    const body = encodeAgentRequest(request)

    let data: unknown
    try {
        const response = await http.post<unknown>(
            `/conversations/${conversationID}/messages`,
            body,
            { signal },
        )
        data = response.data
    } catch (error) {
        rethrowRequestError(error)
    }

    const result = decodeAgentMessageResponse(data)
    if (result.conversation_id !== conversationID) {
        throw new Error('普通响应不属于当前会话')
    }
    return result
}

export async function sendMessageAPI(
    conversationID: number,
    content: string,
    signal?: AbortSignal,
): Promise<AgentMessageResponse> {
    return sendAgentMessageAPI(conversationID, { content }, signal)
}

export async function recoverAgentRunAPI(
    conversationID: number,
    runID: string,
    signal?: AbortSignal,
): Promise<AgentRecovery> {
    positiveID(conversationID)
    uuidValue(runID)

    let data: unknown
    try {
        const response = await http.get<unknown>(
            `/conversations/${conversationID}/agent-runs/${runID}`,
            { signal },
        )
        data = response.data
    } catch (error) {
        if (axios.isAxiosError(error) && error.response !== undefined) {
            const decoded = decodeRequestError(
                error.response.status,
                error.response.data,
            )

            if (
                decoded.details?.recovery !== undefined &&
                decoded.details.recovery.stored.run_id !== runID
            ) {
                throw new Error('恢复错误响应的 Run 与请求不一致')
            }
            throw decoded
        }
        throw error
    }

    const result = decodeAgentRecovery(data)
    if (result.stored.run_id !== runID) {
        throw new Error('恢复响应的 Run 与请求不一致')
    }
    return result
}