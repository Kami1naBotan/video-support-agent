import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { nextTick } from 'vue'
import { createPinia } from 'pinia'
import { flushPromises, mount } from '@vue/test-utils'
import type { VueWrapper } from '@vue/test-utils'
import ElementPlus, { ElMessage } from 'element-plus'

import ChatView from './ChatView.vue'
import AgentPanel from '../components/AgentPanel.vue'
import { useAuthStore } from '../stores/auth'
import { listConversationsAPI } from '../api/conversation'
import { listMessagesAPI, recoverAgentRunAPI } from '../api/message'
import { streamAgentMessageAPI } from '../api/chat-stream'
import type { StreamDoneData } from '../api/chat-stream'
import {
    decodeAgentRecovery,
    decodeAgentResult,
} from '../api/agent-protocol'
import type { AgentRecovery } from '../api/agent-protocol'
import {
    readRecoveryCheckpoint,
    writeRecoveryCheckpoint,
} from '../chat/recovery-checkpoint'
import {
    agentResult, otherRunID, recovery, runID, runSummary,
} from '../test/agent-fixtures'
import {
    uiDone, uiProposal, uiRecovery, withAgentHistory,
} from '../test/agent-ui-fixtures'
import { deferred, message } from '../test/rag-fixtures'

const { replace } = vi.hoisted(() => ({ replace: vi.fn() }))

vi.mock('vue-router', () => ({
    useRouter: () => ({ replace }),
}))
vi.mock('../api/conversation', () => ({
    listConversationsAPI: vi.fn(),
    createConversationAPI: vi.fn(),
}))
vi.mock('../api/message', () => ({
    listMessagesAPI: vi.fn(),
    recoverAgentRunAPI: vi.fn(),
}))
vi.mock('../api/chat-stream', () => ({
    streamAgentMessageAPI: vi.fn(),
}))

let wrapper: VueWrapper | undefined

function savedHistory() {
    return withAgentHistory([
        message({ id: 10, role: 'user', content: 'fixture question' }),
        message({ id: 11, role: 'assistant', content: 'fixture answer' }),
    ])
}

beforeEach(() => {
    vi.resetAllMocks()
    sessionStorage.clear()
    vi.mocked(listConversationsAPI).mockResolvedValue([{
        id: 1,
        user_id: 7,
        title: 'Agent测试会话',
        created_at: '2026-11-04T08:00:00Z',
        updated_at: '2026-11-04T08:00:00Z',
    }])
    vi.mocked(listMessagesAPI).mockResolvedValue([])
    vi.mocked(recoverAgentRunAPI).mockResolvedValue(uiRecovery())
})

afterEach(() => {
    wrapper?.unmount()
    wrapper = undefined
    ElMessage.closeAll()
    vi.restoreAllMocks()
    sessionStorage.clear()
    document.body.replaceChildren()
})

async function render() {
    const pinia = createPinia()
    const auth = useAuthStore(pinia)
    auth.token = 'fixture-token'
    auth.user = { id: 7, username: 'fixture-user', role: 'user' }

    const page = mount(ChatView, {
        attachTo: document.body,
        global: { plugins: [pinia, ElementPlus] },
    })
    wrapper = page
    await flushPromises()
    await nextTick()
    return { page, auth }
}

function button(page: VueWrapper, text: string) {
    const found = page.findAll<HTMLButtonElement>('button')
        .find((item) => item.text() === text)
    if (!found) throw new Error(`missing button: ${text}`)
    return found
}

describe('ChatView Agent lifecycle', () => {
    it('starts with editable empty input', async () => {
        const { page } = await render()
        const input = page.get<HTMLTextAreaElement>('.conversation-panel textarea')
        expect(input.element.value).toBe('')
        expect(input.element.disabled).toBe(false)
        expect(button(page, '发送').element.disabled).toBe(true)

        await input.setValue('可以开始输入')
        expect(button(page, '发送').element.disabled).toBe(false)
        expect(streamAgentMessageAPI).not.toHaveBeenCalled()
    })

    it('recovers before history without reconstructing confirmation', async () => {
        writeRecoveryCheckpoint(7, 1, runID, otherRunID)
        const pending = deferred<AgentRecovery>()
        vi.mocked(recoverAgentRunAPI).mockReturnValueOnce(pending.promise)
        vi.mocked(listMessagesAPI).mockResolvedValue(savedHistory())

        const { page } = await render()
        expect(recoverAgentRunAPI).toHaveBeenCalledExactlyOnceWith(
            1, runID, expect.any(AbortSignal),
        )
        expect(listMessagesAPI).not.toHaveBeenCalled()

        pending.resolve(uiRecovery(true))
        await flushPromises()
        await nextTick()

        expect(listMessagesAPI).toHaveBeenCalledTimes(1)
        expect(page.findAll('.message-item')).toHaveLength(2)
        expect(page.text()).toContain('历史中不包含确认凭据')
        expect(page.find('.confirmation').exists()).toBe(false)
        expect(streamAgentMessageAPI).not.toHaveBeenCalled()
        expect(page.get<HTMLTextAreaElement>('textarea').element.disabled).toBe(false)
    })

    it('confirms once and retries the original operation', async () => {
        const proposal = uiProposal()
        const confirming = deferred<StreamDoneData>()
        const retrying = deferred<StreamDoneData>()
        const recovering = deferred<AgentRecovery>()

        vi.mocked(listMessagesAPI)
            .mockResolvedValueOnce([])
            .mockResolvedValue(savedHistory())
        vi.mocked(recoverAgentRunAPI)
            .mockResolvedValueOnce(uiRecovery(true))
            .mockReturnValueOnce(recovering.promise)

        vi.mocked(streamAgentMessageAPI)
            .mockImplementationOnce(async (_id, _request, options) => {
                options.onEvent({ event: 'start', data: { conversation_id: 1 } })
                options.onEvent({ event: 'agent_start', data: { run_id: runID } })
                options.onEvent({ event: 'action_required', data: proposal })
                options.onEvent({
                    event: 'agent_result',
                    data: decodeAgentResult(agentResult({
                        decision: 'confirmation_required',
                    })),
                })
                const done = uiDone(true)
                options.onEvent({ event: 'done', data: done })
                return done
            })
            .mockReturnValueOnce(confirming.promise)
            .mockReturnValueOnce(retrying.promise)

        const { page } = await render()
        await page.get('.conversation-panel textarea').setValue('fixture question')
        await page.get('.conversation-panel form').trigger('submit')
        await flushPromises()

        expect(streamAgentMessageAPI).toHaveBeenCalledTimes(1)
        expect(vi.mocked(streamAgentMessageAPI).mock.calls[0]?.[1]).toEqual({
            content: 'fixture question',
        })
        expect(button(page, '确认提交人工复核').element.disabled).toBe(true)
        await page.get('input[type="checkbox"]').setValue(true)
        await button(page, '确认提交人工复核').trigger('click')

        // 即使绕过按钮禁用直接触发重复事件，服务调用也不能重复。
        page.getComponent(AgentPanel).vm.$emit('confirm')
        await flushPromises()
        expect(streamAgentMessageAPI).toHaveBeenCalledTimes(2)

        const second = vi.mocked(streamAgentMessageAPI).mock.calls[1]
        if (!second) throw new Error('missing confirmation request')
        expect(second[1]).toEqual({ confirmed_action: proposal.confirmed_action })
        expect(readRecoveryCheckpoint(7, 1)?.operation_key)
            .toBe(proposal.confirmed_action.idempotency_key)

        second[2].onEvent({
            event: 'agent_start',
            data: { run_id: otherRunID },
        })
        confirming.reject(new DOMException('aborted', 'AbortError'))
        await flushPromises()

        expect(recoverAgentRunAPI).toHaveBeenCalledTimes(2)
        expect(streamAgentMessageAPI).toHaveBeenCalledTimes(2)
        expect(page.text()).toContain('请勿重复提交')

        recovering.resolve(decodeAgentRecovery(recovery({
            stored: runSummary({
                run_id: otherRunID,
                decision: 'failed',
                message_save_status: 'not_attempted',
            }),
            message_save_status: 'not_attempted',
            messages: undefined,
        })))
        await flushPromises()
        await nextTick()

        expect(button(page, '使用原申请重试').element.disabled).toBe(true)
        await page.get('input[type="checkbox"]').setValue(true)
        await button(page, '使用原申请重试').trigger('click')
        await flushPromises()

        expect(streamAgentMessageAPI).toHaveBeenCalledTimes(3)
        expect(vi.mocked(streamAgentMessageAPI).mock.calls[2]?.[1]).toEqual(second[1])

        const third = vi.mocked(streamAgentMessageAPI).mock.calls[2]
        page.unmount()
        wrapper = undefined
        expect(third?.[2].signal.aborted).toBe(true)
        retrying.reject(new DOMException('aborted', 'AbortError'))
        await flushPromises()
    })

    it('ignores recovery after logout and clears checkpoints', async () => {
        writeRecoveryCheckpoint(7, 1, runID)
        const pending = deferred<AgentRecovery>()
        vi.mocked(recoverAgentRunAPI).mockReturnValueOnce(pending.promise)

        const { page, auth } = await render()
        const signal = vi.mocked(recoverAgentRunAPI).mock.calls[0]?.[2]
        expect(signal?.aborted).toBe(false)

        auth.logout()
        expect(signal?.aborted).toBe(true)
        expect(readRecoveryCheckpoint(7, 1)).toBeUndefined()
        expect(auth.token).toBe('')
        expect(auth.user).toBeNull()

        // 模拟新身份已建立，而旧恢复请求仍迟到返回。
        auth.token = 'new-session'
        auth.user = { id: 8, username: 'other-user', role: 'user' }
        pending.resolve(uiRecovery())
        await flushPromises()

        expect(listMessagesAPI).not.toHaveBeenCalled()
        expect(page.findAll('.message-item')).toHaveLength(0)
        expect(page.text()).not.toContain('已完成本次执行的恢复核对')
        expect(streamAgentMessageAPI).not.toHaveBeenCalled()
        expect(auth.token).toBe('new-session')
        expect(replace).not.toHaveBeenCalled()
    })
})