import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { nextTick } from 'vue'
import { createPinia } from 'pinia'
import { flushPromises, mount } from '@vue/test-utils'
import type { VueWrapper } from '@vue/test-utils'
import ElementPlus, { ElMessage } from 'element-plus'

import ChatView from './ChatView.vue'
import { useAuthStore } from '../stores/auth'
import { listConversationsAPI } from '../api/conversation'
import { listMessagesAPI } from '../api/message'
import type { ChatMessage } from '../api/message'
import { streamMessageAPI } from '../api/chat-stream'
import type { StreamDoneData } from '../api/chat-stream'
import { getKnowledgeArticleAPI } from '../api/knowledge-article'
import type { KnowledgeArticleDetail } from '../api/knowledge-article'
import { ChatStreamRequestError } from '../api/chat-stream-error'
import { citation, deferred, message } from '../test/rag-fixtures'

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
}))
vi.mock('../api/chat-stream', () => ({
    streamMessageAPI: vi.fn(),
}))
vi.mock('../api/knowledge-article', () => ({
    getKnowledgeArticleAPI: vi.fn(),
}))

let wrapper: VueWrapper | undefined

beforeEach(() => {
    vi.resetAllMocks()
    sessionStorage.clear()

    vi.spyOn(ElMessage, 'warning').mockImplementation(() => ({
        close: () => {},
    }))

    vi.mocked(listConversationsAPI).mockResolvedValue(
        [1, 2].map((id) => ({
            id,
            user_id: 7,
            title: `会话 ${id}`,
            created_at: '2026-09-01T00:00:00Z',
            updated_at: '2026-09-01T00:00:00Z',
        })),
    )
    vi.mocked(listMessagesAPI).mockResolvedValue([])
})

afterEach(() => {
    wrapper?.unmount()
    wrapper = undefined
    ElMessage.closeAll()
    vi.restoreAllMocks()
    document.body.replaceChildren()
    sessionStorage.clear()
})

async function render() {
    const pinia = createPinia()
    const auth = useAuthStore(pinia)
    auth.token = 'test-only-token'
    auth.user = { id: 7, username: 'test-user', role: 'user' }

    const page = mount(ChatView, {
        attachTo: document.body,
        global: { plugins: [pinia, ElementPlus] },
    })
    wrapper = page
    await flushPromises()
    await nextTick()

    expect(page.get('.conversation-panel h2').text()).toBe('会话 1')
    return { page, auth }
}

async function submit(page: VueWrapper) {
    await page.get('.conversation-panel textarea').setValue('虚构问题')
    await page.get('.conversation-panel form').trigger('submit')
    await flushPromises()
}

function options() {
    const call = vi.mocked(streamMessageAPI).mock.calls.at(-1)
    if (!call) throw new Error('没有流式请求')
    return call[2]
}

function button(page: VueWrapper, label: string) {
    const found = page.findAll('button').find((item) => item.text() === label)
    if (!found) throw new Error(`缺少按钮：${label}`)
    return found
}

const done: StreamDoneData = {
    user_message_id: 10,
    assistant_message_id: 11,
}

function savedMessages(): ChatMessage[] {
    return [
        message({ id: 10, role: 'user', content: '虚构问题' }),
        message({ citations: [citation()] }),
    ]
}

describe('ChatView citations', () => {
    it('history citations', async () => {
        const legacy = message({ id: 12, content: '旧消息' })
        delete legacy.citations

        vi.mocked(listMessagesAPI).mockResolvedValueOnce([
            message({ id: 10, role: 'user', content: '虚构问题' }),
            message({
                content: '<em>虚构回答</em> [R1]',
                citations: [citation()],
            }),
            legacy,
        ])

        const { page } = await render()

        expect(page.findAll('.message-item')).toHaveLength(3)
        expect(page.findAll('.rule-citations')).toHaveLength(1)
        expect(page.find('.from-user .rule-citations').exists()).toBe(false)
        expect(page.get('.citation-list strong').text()).toBe('[R1] 虚构测试规则1')
        expect(page.text()).toContain('当前状态未核实')
        expect(page.find('.message-content em').exists()).toBe(false)
        expect(page.findAll('.message-content')[1]!.element.textContent).toBe(
            '<em>虚构回答</em> [R1]',
        )
        expect(getKnowledgeArticleAPI).not.toHaveBeenCalled()
        expect(streamMessageAPI).not.toHaveBeenCalled()
    })

    it('live citations and done', async () => {
        const stream = deferred<StreamDoneData>()
        const history = deferred<ChatMessage[]>()
        vi.mocked(streamMessageAPI).mockReturnValueOnce(stream.promise)
        vi.mocked(listMessagesAPI)
            .mockResolvedValueOnce([])
            .mockReturnValueOnce(history.promise)

        const { page } = await render()
        await submit(page)
        const callbacks = options()

        callbacks.onEvent({ event: 'start', data: { conversation_id: 1 } })
        callbacks.onEvent({
            event: 'token',
            data: { content: '虚构回答 [R1]' },
        })
        callbacks.onEvent({
            event: 'citation',
            data: { items: [citation()] },
        })
        await nextTick()

        expect(page.findAll('.rule-citations')).toHaveLength(1)
        expect(page.text()).toContain('当前状态未核实')
        expect(page.text()).not.toContain('回答已完成并保存')
        expect(listMessagesAPI).toHaveBeenCalledTimes(1)

        // 收到 done 回调仍不立即宣布完成，等待请求层成功返回。
        callbacks.onEvent({ event: 'done', data: done })
        await nextTick()
        expect(page.text()).not.toContain('回答已完成并保存')
        expect(listMessagesAPI).toHaveBeenCalledTimes(1)

        stream.resolve(done)
        await flushPromises()
        expect(page.text()).toContain('回答已完成并保存')
        expect(listMessagesAPI).toHaveBeenCalledTimes(2)
        expect(page.findAll('.rule-citations')).toHaveLength(1)

        history.resolve(savedMessages())
        await flushPromises()

        expect(page.findAll('.message-item')).toHaveLength(2)
        expect(page.findAll('.rule-citations')).toHaveLength(1)
        expect(page.get('.citation-list strong').text()).toBe('[R1] 虚构测试规则1')
        expect(page.find('.message-list .stream-status').exists()).toBe(false)
        expect(page.text()).toContain('回答已完成并保存')
        expect(getKnowledgeArticleAPI).not.toHaveBeenCalled()
    })

    it('history recovery', async () => {
        const stream = deferred<StreamDoneData>()
        vi.mocked(streamMessageAPI).mockReturnValueOnce(stream.promise)
        vi.mocked(listMessagesAPI)
            .mockResolvedValueOnce([])
            .mockRejectedValueOnce(new Error('test-only history failure'))
            .mockResolvedValueOnce(savedMessages())

        const { page } = await render()
        await submit(page)
        const callbacks = options()
        callbacks.onEvent({ event: 'start', data: { conversation_id: 1 } })
        callbacks.onEvent({
            event: 'token',
            data: { content: '虚构回答 [R1]' },
        })
        callbacks.onEvent({
            event: 'citation',
            data: { items: [citation()] },
        })
        callbacks.onEvent({ event: 'done', data: done })
        stream.resolve(done)
        await flushPromises()

        expect(page.text()).toContain('加载消息失败')
        expect(page.text()).toContain('回答已完成并保存')
        expect(page.findAll('.rule-citations')).toHaveLength(1)

        await submit(page)
        expect(streamMessageAPI).toHaveBeenCalledTimes(1)

        await button(page, '刷新消息').trigger('click')
        await flushPromises()

        expect(listMessagesAPI).toHaveBeenCalledTimes(3)
        expect(page.text()).not.toContain('加载消息失败')
        expect(page.findAll('.message-item')).toHaveLength(2)
        expect(page.findAll('.rule-citations')).toHaveLength(1)
        expect(page.get('.citation-list strong').text()).toBe('[R1] 虚构测试规则1')
    })

    it('stale page results', async () => {
        for (const mode of ['switch', 'unmount', 'auth']) {
            const stream = deferred<StreamDoneData>()
            vi.mocked(streamMessageAPI).mockReturnValueOnce(stream.promise)

            const { page, auth } = await render()
            await submit(page)
            const callbacks = options()

            if (mode === 'switch') {
                await button(page, '会话 2').trigger('click')
                await flushPromises()
            } else if (mode === 'unmount') {
                page.unmount()
                wrapper = undefined
            } else {
                auth.logout()
                auth.token = 'new-session'
                await nextTick()
            }

            expect(callbacks.signal.aborted, mode).toBe(true)
            const historyCalls = vi.mocked(listMessagesAPI).mock.calls.length

            callbacks.onEvent({
                event: 'citation',
                data: { items: [citation(2)] },
            })
            callbacks.onEvent({ event: 'done', data: done })
            stream.reject(new ChatStreamRequestError(
                401, 'unauthorized', 'test-only stale stream 401',
            ))
            await flushPromises()

            expect(listMessagesAPI).toHaveBeenCalledTimes(historyCalls)
            expect(replace).not.toHaveBeenCalled()

            if (mode !== 'unmount') {
                expect(page.text()).not.toContain('虚构测试规则2')
                expect(page.text()).not.toContain('回答已完成并保存')
                page.unmount()
                wrapper = undefined
            }
            if (mode === 'auth') expect(auth.token).toBe('new-session')
        }
    })

    it('citation unauthorized', async () => {
        for (const mode of ['current', 'stale']) {
            replace.mockClear()
            const detail = deferred<KnowledgeArticleDetail>()
            vi.mocked(listMessagesAPI).mockResolvedValueOnce(savedMessages())
            vi.mocked(getKnowledgeArticleAPI).mockReturnValueOnce(detail.promise)

            const { page, auth } = await render()
            await page.get('.citation-list button').trigger('click')

            if (mode === 'stale') {
                auth.logout()
                auth.token = 'new-session'
                await nextTick()
            }

            detail.reject(new ChatStreamRequestError(
                401, 'unauthorized', 'test-only detail 401',
            ))
            await flushPromises()

            if (mode === 'current') {
                expect(auth.token).toBe('')
                expect(replace).toHaveBeenCalledExactlyOnceWith('/login')
            } else {
                expect(auth.token).toBe('new-session')
                expect(replace).not.toHaveBeenCalled()
            }

            page.unmount()
            wrapper = undefined
        }
    })
})