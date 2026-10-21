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
import ElementPlus from 'element-plus'

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

  const promise = new Promise<T>((resolvePromise) => {
    resolve = resolvePromise
  })

  return { promise, resolve }
}

let wrapper: VueWrapper | undefined

beforeEach(() => {
  vi.resetAllMocks()
  sessionStorage.clear()
})

afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
  document.body.replaceChildren()
  sessionStorage.clear()
})

describe('ChatView streaming', () => {
  it('renders chunks before completion and replaces the temporary turn with history', async () => {
    const question = '请介绍客服可以提供的帮助'
    const firstPart = '你好，'
    const secondPart = '\n可以帮你了解规则。'
    const answer = firstPart + secondPart

    const stream = deferred<StreamDoneData>()
    const history = deferred<ChatMessage[]>()

    vi.mocked(listConversationsAPI).mockResolvedValue([
      {
        id: 1,
        user_id: 7,
        title: '测试会话',
        created_at: '2026-10-21T00:00:00Z',
        updated_at: '2026-10-21T00:00:00Z',
      },
    ])

    vi.mocked(listMessagesAPI)
        .mockResolvedValueOnce([])
        .mockImplementationOnce(() => history.promise)

    vi.mocked(streamMessageAPI).mockImplementation(
        () => stream.promise,
    )

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

    // 等待挂载后的会话查询及自动选择会话触发的历史查询。
    await flushPromises()
    await nextTick()

    expect(page.get('.conversation-panel h2').text()).toBe(
        '测试会话',
    )
    expect(listMessagesAPI).toHaveBeenCalledTimes(1)
    expect(page.findAll('.message-item')).toHaveLength(0)

    const input = page.get<HTMLTextAreaElement>(
        '.conversation-panel textarea',
    )
    await input.setValue(question)

    await page.get('.conversation-panel form').trigger('submit')
    await flushPromises()

    expect(streamMessageAPI).toHaveBeenCalledTimes(1)

    const call = vi.mocked(streamMessageAPI).mock.calls[0]
    if (!call) {
      throw new Error('页面没有调用流式 API')
    }

    const [conversationID, submittedContent, options] = call

    expect(conversationID).toBe(1)
    expect(submittedContent).toBe(question)
    expect(options.signal.aborted).toBe(false)

    expect(page.text()).toContain('正在等待客服回答')
    expect(input.element.disabled).toBe(true)

    const sendButton = page.get<HTMLButtonElement>(
        '.send-actions button[type="submit"]',
    )
    expect(sendButton.element.disabled).toBe(true)
    expect(sendButton.classes()).toContain('is-loading')

    // 请求还没有完成，页面先显示等待状态。
    expect(page.text()).not.toContain('回答已完成并保存')
    expect(listMessagesAPI).toHaveBeenCalledTimes(1)

    options.onEvent({
      event: 'start',
      data: { conversation_id: 1 },
    })
    await nextTick()

    expect(page.text()).toContain('正在生成回答')

    options.onEvent({
      event: 'token',
      data: { content: firstPart },
    })
    await nextTick()

    const answerSelector =
        '.message-item:not(.from-user) .message-content'

    expect(
        page.get(answerSelector).element.textContent,
    ).toBe(firstPart)

    options.onEvent({
      event: 'token',
      data: { content: secondPart },
    })
    await nextTick()

    // 通过 textContent 检查原始换行，避免 text() 的修剪影响断言。
    expect(
        page.get(answerSelector).element.textContent,
    ).toBe(answer)

    expect(page.findAll('.message-item')).toHaveLength(2)
    expect(page.text()).not.toContain('回答已完成并保存')
    expect(listMessagesAPI).toHaveBeenCalledTimes(1)

    options.onEvent({
      event: 'citation',
      data: { items: [] },
    })

    const done: StreamDoneData = {
      user_message_id: 10,
      assistant_message_id: 11,
    }

    options.onEvent({
      event: 'done',
      data: done,
    })

    // 请求层确认协议正常完成后，才正常返回。
    stream.resolve(done)
    await flushPromises()
    await nextTick()

    expect(listMessagesAPI).toHaveBeenCalledTimes(2)
    expect(listMessagesAPI).toHaveBeenLastCalledWith(
        1,
        expect.any(AbortSignal),
    )
    expect(page.text()).toContain('回答已完成并保存')
    expect(page.text()).toContain('正在加载消息')

    // 历史查询尚未完成，不能提前删除临时回答。
    expect(
        page.get(answerSelector).element.textContent,
    ).toBe(answer)
    expect(page.findAll('.message-item')).toHaveLength(2)
    expect(input.element.disabled).toBe(true)

    history.resolve([
      {
        id: 10,
        conversation_id: 1,
        role: 'user',
        content: question,
        created_at: '2026-10-21T00:00:01Z',
      },
      {
        id: 11,
        conversation_id: 1,
        role: 'assistant',
        content: answer,
        created_at: '2026-10-21T00:00:01Z',
      },
    ])

    await flushPromises()
    await nextTick()

    const contents = page
        .findAll('.message-item .message-content')
        .map((item) => item.element.textContent)

    // 历史替换临时问答，而不是追加成四条消息。
    expect(contents).toEqual([question, answer])
    expect(page.findAll('.message-item')).toHaveLength(2)

    // 临时问答内部的状态提示消失，页面级完成提示仍保留。
    expect(page.find('.message-list .stream-status').exists()).toBe(
        false,
    )
    expect(page.text()).toContain('回答已完成并保存')
    expect(page.text()).not.toContain('正在加载消息')
    expect(page.text()).not.toContain('正在生成回答')

    expect(input.element.disabled).toBe(false)
    expect(input.element.value).toBe('')
    expect(sendButton.classes()).not.toContain('is-loading')

    // 输入框为空时，发送按钮仍禁用，这是正常行为。
    await input.setValue('下一个问题')
    expect(sendButton.element.disabled).toBe(false)

    // 仅验证恢复输入能力，不自动发送第二轮。
    expect(streamMessageAPI).toHaveBeenCalledTimes(1)
  })
})