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
            title: '会话 A',
            created_at: '2026-10-21T00:00:00Z',
            updated_at: '2026-10-21T00:00:00Z',
        },
        {
            id: 2,
            user_id: 7,
            title: '会话 B',
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

    expect(page.get('.conversation-panel h2').text()).toBe('会话 A')
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

function actionButton(page: VueWrapper, label: string) {
    const button = page
        .findAll<HTMLButtonElement>('.send-actions button')
        .find((item) => item.text() === label)

    if (!button) {
        throw new Error(`找不到按钮：${label}`)
    }
    return button
}

function displayedContents(page: VueWrapper) {
    return page
        .findAll('.message-item .message-content')
        .map((item) => item.element.textContent)
}

describe('ChatView stream cancellation', () => {
    it('aborts on stop, preserves partial text and finishes loading without retry', async () => {
        const pending = deferred<StreamDoneData>()

        vi.mocked(streamMessageAPI).mockReturnValue(pending.promise)

        const errorToast = vi.spyOn(ElMessage, 'error')
            .mockImplementation(() => ({ close: () => {} }))

        const page = await mountChatPage()
        await submitQuestion(page, '问题 A')

        expect(streamMessageAPI).toHaveBeenCalledTimes(1)
        const options = streamOptions(0)

        options.onEvent({
            event: 'start',
            data: { conversation_id: 1 },
        })
        options.onEvent({
            event: 'token',
            data: { content: '已经收到的部分回答' },
        })
        await nextTick()

        expect(displayedContents(page)).toEqual([
            '问题 A',
            '已经收到的部分回答',
        ])

        await actionButton(page, '停止生成').trigger('click')

        expect(options.signal.aborted).toBe(true)
        expect(page.text()).toContain('已停止接收')
        expect(page.text()).not.toContain('回答已完成并保存')

        // 此时模拟请求尚未结束清理，页面仍处于 sending。
        const sendButton = page.get<HTMLButtonElement>(
            '.send-actions button[type="submit"]',
        )
        expect(sendButton.classes()).toContain('is-loading')

        // 即使有迟到片段，已取消的请求也不能继续追加文字。
        options.onEvent({
            event: 'token',
            data: { content: '不应追加的迟到文字' },
        })
        await nextTick()

        expect(displayedContents(page)).toEqual([
            '问题 A',
            '已经收到的部分回答',
        ])

        // 模拟请求层完成取消和资源清理后，以 AbortError 拒绝。
        pending.reject(new DOMException('aborted', 'AbortError'))
        await flushPromises()
        await nextTick()

        expect(sendButton.classes()).not.toContain('is-loading')
        expect(
            page.get<HTMLTextAreaElement>(
                '.conversation-panel textarea',
            ).element.disabled,
        ).toBe(false)

        expect(page.text()).toContain('已停止接收')
        expect(page.text()).not.toContain('本轮回答未确认完成')
        expect(page.text()).not.toContain('回答已完成并保存')
        expect(displayedContents(page)).toEqual([
            '问题 A',
            '已经收到的部分回答',
        ])

        expect(errorToast).not.toHaveBeenCalled()
        expect(streamMessageAPI).toHaveBeenCalledTimes(1)

        // 停止后没有自动查询历史，也没有重新提交问题。
        expect(listMessagesAPI).toHaveBeenCalledTimes(1)
    })

    it('ignores old callbacks and cleanup after switching to a new stream', async () => {
        const oldStream = deferred<StreamDoneData>()
        const newStream = deferred<StreamDoneData>()

        vi.mocked(streamMessageAPI)
            .mockReturnValueOnce(oldStream.promise)
            .mockReturnValueOnce(newStream.promise)

        const page = await mountChatPage()
        await submitQuestion(page, '问题 A')

        const oldOptions = streamOptions(0)
        oldOptions.onEvent({
            event: 'start',
            data: { conversation_id: 1 },
        })
        oldOptions.onEvent({
            event: 'token',
            data: { content: '会话 A 的部分回答' },
        })
        await nextTick()

        const conversationB = page
            .findAll('.conversation-item')
            .find((item) => item.text() === '会话 B')

        if (!conversationB) {
            throw new Error('找不到会话 B')
        }

        await conversationB.trigger('click')
        await flushPromises()
        await nextTick()

        expect(oldOptions.signal.aborted).toBe(true)
        expect(page.get('.conversation-panel h2').text()).toBe('会话 B')
        expect(page.findAll('.message-item')).toHaveLength(0)

        await submitQuestion(page, '问题 B')

        expect(streamMessageAPI).toHaveBeenCalledTimes(2)
        expect(vi.mocked(streamMessageAPI).mock.calls[1]?.[0]).toBe(2)

        const newOptions = streamOptions(1)
        expect(newOptions.signal.aborted).toBe(false)

        newOptions.onEvent({
            event: 'start',
            data: { conversation_id: 2 },
        })
        newOptions.onEvent({
            event: 'token',
            data: { content: '会话 B 的第一段' },
        })
        await nextTick()

        // 故意模拟不及时响应取消的旧请求，检验页面自身的防护。
        oldOptions.onEvent({
            event: 'token',
            data: { content: '会话 A 的迟到文字' },
        })

        const oldDone: StreamDoneData = {
            user_message_id: 10,
            assistant_message_id: 11,
        }
        oldOptions.onEvent({
            event: 'done',
            data: oldDone,
        })
        oldStream.resolve(oldDone)

        await flushPromises()
        await nextTick()

        expect(displayedContents(page)).toEqual([
            '问题 B',
            '会话 B 的第一段',
        ])
        expect(page.text()).not.toContain('回答已完成并保存')

        // 旧请求的 finally 不能结束新请求的加载状态。
        const sendButton = page.get<HTMLButtonElement>(
            '.send-actions button[type="submit"]',
        )
        expect(sendButton.classes()).toContain('is-loading')
        expect(sendButton.element.disabled).toBe(true)
        expect(
            page.get<HTMLTextAreaElement>(
                '.conversation-panel textarea',
            ).element.disabled,
        ).toBe(true)
        expect(newOptions.signal.aborted).toBe(false)

        // 只查询过 A、B 的初始历史；旧请求结束不得触发额外刷新。
        expect(listMessagesAPI).toHaveBeenCalledTimes(2)

        newOptions.onEvent({
            event: 'token',
            data: { content: '\n会话 B 的第二段' },
        })
        await nextTick()

        expect(displayedContents(page)).toEqual([
            '问题 B',
            '会话 B 的第一段\n会话 B 的第二段',
        ])

        // 通过停止按钮验证：当前控制器仍属于 B，没有被旧 finally 清空。
        await actionButton(page, '停止生成').trigger('click')
        expect(newOptions.signal.aborted).toBe(true)

        newStream.reject(new DOMException('aborted', 'AbortError'))
        await flushPromises()
        await nextTick()

        expect(sendButton.classes()).not.toContain('is-loading')
        expect(page.text()).toContain('已停止接收')
        expect(streamMessageAPI).toHaveBeenCalledTimes(2)
    })
})