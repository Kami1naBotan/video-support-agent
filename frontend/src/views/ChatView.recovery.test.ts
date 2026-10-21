import {
    afterEach,
    beforeEach,
    describe,
    expect,
    it,
    vi,
} from 'vitest'
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

vi.mock('vue-router', () => ({
    useRouter: () => ({
        replace: vi.fn(),
    }),
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

function deferred<T>() {
    let resolve!: (value: T) => void
    let reject!: (reason?: unknown) => void

    const promise = new Promise<T>((resolvePromise, rejectPromise) => {
        resolve = resolvePromise
        reject = rejectPromise
    })

    return { promise, resolve, reject }
}

let wrapper: VueWrapper | undefined

beforeEach(() => {
    vi.resetAllMocks()
    sessionStorage.clear()

    vi.mocked(listConversationsAPI).mockResolvedValue([
        {
            id: 1,
            user_id: 7,
            title: '测试会话',
            created_at: '2026-10-21T00:00:00Z',
            updated_at: '2026-10-21T00:00:00Z',
        },
    ])

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

async function mountChatPage() {
    const pinia = createPinia()
    const auth = useAuthStore(pinia)

    auth.token = 'test-only-token'
    auth.user = {
        id: 7,
        username: 'test-user',
        role: 'user',
    }

    const page = mount(ChatView, {
        attachTo: document.body,
        global: {
            plugins: [pinia, ElementPlus],
        },
    })
    wrapper = page

    await flushPromises()
    await nextTick()

    expect(page.get('.conversation-panel h2').text()).toBe('测试会话')
    return page
}

async function submitQuestion(page: VueWrapper, question: string) {
    await page
        .get<HTMLTextAreaElement>('.conversation-panel textarea')
        .setValue(question)

    await page.get('.conversation-panel form').trigger('submit')
    await flushPromises()
}

function streamOptions(index: number) {
    const call = vi.mocked(streamMessageAPI).mock.calls[index]
    if (!call) {
        throw new Error(`缺少第 ${index + 1} 次流式请求`)
    }
    return call[2]
}

function displayedContents(page: VueWrapper) {
    return page
        .findAll('.message-item .message-content')
        .map((item) => item.element.textContent)
}

describe('ChatView recovery and unmount', () => {
    it('preserves the saved answer after history failure and recovers on manual refresh', async () => {
        const firstStream = deferred<StreamDoneData>()
        const secondStream = deferred<StreamDoneData>()
        const refreshedHistory = deferred<ChatMessage[]>()

        vi.mocked(streamMessageAPI)
            .mockReturnValueOnce(firstStream.promise)
            .mockReturnValueOnce(secondStream.promise)

        vi.mocked(listMessagesAPI)
            .mockResolvedValueOnce([])
            .mockRejectedValueOnce(new Error('test history failure'))
            .mockImplementationOnce(() => refreshedHistory.promise)

        const warningToast = vi.spyOn(ElMessage, 'warning')
            .mockImplementation(() => ({ close: () => {} }))
        const errorToast = vi.spyOn(ElMessage, 'error')
            .mockImplementation(() => ({ close: () => {} }))

        const page = await mountChatPage()
        await submitQuestion(page, '第一个问题')

        const options = streamOptions(0)
        options.onEvent({
            event: 'start',
            data: { conversation_id: 1 },
        })
        options.onEvent({
            event: 'token',
            data: { content: '已经完整生成的回答' },
        })

        const done: StreamDoneData = {
            user_message_id: 10,
            assistant_message_id: 11,
        }
        options.onEvent({ event: 'done', data: done })
        firstStream.resolve(done)

        await flushPromises()
        await nextTick()

        expect(listMessagesAPI).toHaveBeenCalledTimes(2)
        expect(page.text()).toContain('加载消息失败')
        expect(page.text()).toContain('回答已完成并保存')
        expect(page.text()).not.toContain('本轮回答未确认完成')
        expect(displayedContents(page)).toEqual([
            '第一个问题',
            '已经完整生成的回答',
        ])

        const sendButton = page.get<HTMLButtonElement>(
            '.send-actions button[type="submit"]',
        )
        expect(sendButton.classes()).not.toContain('is-loading')

        // 历史未同步时，输入可以保留，但不能直接提交新一轮。
        await submitQuestion(page, '第二个问题')

        expect(streamMessageAPI).toHaveBeenCalledTimes(1)
        expect(warningToast).toHaveBeenCalledWith(
            '请先刷新消息确认上一轮结果，再发送新消息',
        )
        expect(errorToast).not.toHaveBeenCalled()

        const refreshButton = page
            .findAll<HTMLButtonElement>('.conversation-panel button')
            .find((item) => item.text() === '刷新消息')

        if (!refreshButton) {
            throw new Error('找不到刷新消息按钮')
        }

        await refreshButton.trigger('click')
        await flushPromises()
        await nextTick()

        expect(listMessagesAPI).toHaveBeenCalledTimes(3)
        expect(page.text()).toContain('正在加载消息')

        // 手动刷新尚未完成时，仍然保留完整回答。
        expect(displayedContents(page)).toEqual([
            '第一个问题',
            '已经完整生成的回答',
        ])

        refreshedHistory.resolve([
            {
                id: 10,
                conversation_id: 1,
                role: 'user',
                content: '第一个问题',
                created_at: '2026-10-21T00:00:01Z',
            },
            {
                id: 11,
                conversation_id: 1,
                role: 'assistant',
                content: '已经完整生成的回答',
                created_at: '2026-10-21T00:00:01Z',
            },
        ])

        await flushPromises()
        await nextTick()

        expect(page.text()).not.toContain('加载消息失败')
        expect(page.text()).not.toContain('正在加载消息')
        expect(page.text()).toContain('回答已完成并保存')
        expect(displayedContents(page)).toEqual([
            '第一个问题',
            '已经完整生成的回答',
        ])
        expect(page.findAll('.message-item')).toHaveLength(2)
        expect(page.find('.message-list .stream-status').exists()).toBe(false)

        const input = page.get<HTMLTextAreaElement>(
            '.conversation-panel textarea',
        )
        expect(input.element.value).toBe('第二个问题')
        expect(input.element.disabled).toBe(false)

        // 刷新成功后，新一轮提交恢复正常。
        await page.get('.conversation-panel form').trigger('submit')
        await flushPromises()

        expect(streamMessageAPI).toHaveBeenCalledTimes(2)
        expect(vi.mocked(streamMessageAPI).mock.calls[1]?.[1]).toBe(
            '第二个问题',
        )

        // 结束测试中尚未完成的第二轮，避免留下悬而未决的请求。
        const secondOptions = streamOptions(1)
        page.unmount()
        wrapper = undefined

        expect(secondOptions.signal.aborted).toBe(true)
        secondStream.reject(new DOMException('aborted', 'AbortError'))
        await flushPromises()
    })

    it('aborts an active stream on unmount and ignores its late completion', async () => {
        const pending = deferred<StreamDoneData>()
        vi.mocked(streamMessageAPI).mockReturnValue(pending.promise)

        const errorToast = vi.spyOn(ElMessage, 'error')
            .mockImplementation(() => ({ close: () => {} }))

        const page = await mountChatPage()
        await submitQuestion(page, '测试问题')

        const options = streamOptions(0)
        options.onEvent({
            event: 'start',
            data: { conversation_id: 1 },
        })
        options.onEvent({
            event: 'token',
            data: { content: '部分回答' },
        })
        await nextTick()

        expect(displayedContents(page)).toEqual([
            '测试问题',
            '部分回答',
        ])

        page.unmount()
        wrapper = undefined

        expect(options.signal.aborted).toBe(true)

        // 故意模拟取消后仍到达的旧回调和正常返回。
        options.onEvent({
            event: 'token',
            data: { content: '迟到内容' },
        })

        const done: StreamDoneData = {
            user_message_id: 10,
            assistant_message_id: 11,
        }
        options.onEvent({ event: 'done', data: done })
        pending.resolve(done)

        await flushPromises()
        await nextTick()

        expect(document.querySelector('.chat-page')).toBeNull()
        expect(listMessagesAPI).toHaveBeenCalledTimes(1)
        expect(streamMessageAPI).toHaveBeenCalledTimes(1)
        expect(errorToast).not.toHaveBeenCalled()
    })

    it('aborts a pending history request when the page unmounts', async () => {
        const history = deferred<ChatMessage[]>()
        vi.mocked(listMessagesAPI).mockReturnValue(history.promise)

        const errorToast = vi.spyOn(ElMessage, 'error')
            .mockImplementation(() => ({ close: () => {} }))

        const page = await mountChatPage()
        expect(page.text()).toContain('正在加载消息')
        expect(listMessagesAPI).toHaveBeenCalledTimes(1)

        const signal = vi.mocked(listMessagesAPI).mock.calls[0]?.[1]
        if (!signal) {
            throw new Error('历史查询没有携带取消信号')
        }

        expect(signal.aborted).toBe(false)

        page.unmount()
        wrapper = undefined

        expect(signal.aborted).toBe(true)

        history.reject(new DOMException('aborted', 'AbortError'))
        await flushPromises()
        await nextTick()

        expect(document.querySelector('.chat-page')).toBeNull()
        expect(errorToast).not.toHaveBeenCalled()
        expect(streamMessageAPI).not.toHaveBeenCalled()
    })
})