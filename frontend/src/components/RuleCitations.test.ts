import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { nextTick } from 'vue'
import { createPinia } from 'pinia'
import { flushPromises, mount } from '@vue/test-utils'
import type { VueWrapper } from '@vue/test-utils'

import RuleCitations from './RuleCitations.vue'
import { getKnowledgeArticleAPI } from '../api/knowledge-article'
import type { KnowledgeArticleDetail } from '../api/knowledge-article'
import type { RuleCitation } from '../api/rule-citation'
import { ChatStreamRequestError } from '../api/chat-stream-error'
import { useAuthStore } from '../stores/auth'
import { article, citation, deferred } from '../test/rag-fixtures'

vi.mock('../api/knowledge-article', () => ({
    getKnowledgeArticleAPI: vi.fn(),
}))

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

function render(items: RuleCitation[] = [citation()]) {
    const pinia = createPinia()
    const auth = useAuthStore(pinia)
    auth.token = 'test-only-token'

    const view = mount(RuleCitations, {
        props: { items },
        attachTo: document.body,
        global: { plugins: [pinia] },
    })
    wrapper = view
    return { view, auth }
}

function button(view: VueWrapper, label: string) {
    const found = view.findAll<HTMLButtonElement>('button')
        .find((item) => item.text() === label)
    if (!found) throw new Error(`缺少按钮：${label}`)
    return found
}

async function open(view: VueWrapper, index = 0) {
    const target = view.findAll('.citation-list > li button')[index]
    if (!target) throw new Error('缺少引用按钮')
    await target.trigger('click')
}

function lastSignal() {
    const call = vi.mocked(getKnowledgeArticleAPI).mock.calls.at(-1)
    if (!call) throw new Error('没有条款查询')
    return call[1]
}

describe('RuleCitations', () => {
    it('snapshot rendering', async () => {
        const { view } = render([])
        expect(view.find('.rule-citations').exists()).toBe(false)

        const item = { ...citation(), title: '<em>虚构规则</em>' }
        await view.setProps({ items: [item] })

        expect(view.get('.citation-list strong').text()).toBe(
            '[R1] <em>虚构规则</em>',
        )
        expect(view.find('.citation-list em').exists()).toBe(false)
        expect(view.text()).toContain('原文未编号')
        expect(view.text()).toContain('未注明固定截止日期')
        expect(view.text()).toContain('检索时间')
        expect(view.text()).toContain('当前状态未核实')
        expect(view.text()).not.toContain('查询时为当前有效版本')
        expect(getKnowledgeArticleAPI).not.toHaveBeenCalled()

        const link = view.get('a')
        expect(link.attributes('href')).toBe(item.source_url)
        expect(link.attributes('target')).toBe('_blank')
        expect(link.attributes('rel')).toBe('noopener noreferrer')

        await view.setProps({
            items: [{ ...item, source_url: 'https://example.test/rule' }],
        })
        expect(view.find('a').exists()).toBe(false)
        expect(view.text()).toContain('来源链接不可用')
    })

    it('state labels', async () => {
        const cases: Array<[KnowledgeArticleDetail, string[]]> = [
            [
                article(),
                ['查询时为当前有效版本', '当前选定版本'],
            ],
            [
                article({ is_current_effective: false }),
                ['查询时未标记为当前有效版本', '当前选定版本'],
            ],
            [
                article({ is_current: false, is_current_effective: false }),
                ['查询时未标记为当前有效版本'],
            ],
            [
                article({
                    is_current: false,
                    is_current_effective: false,
                    is_superseded: true,
                    is_expired: true,
                    is_withdrawn: true,
                }),
                ['查询时未标记为当前有效版本', '已被替代', '已过期', '已下线'],
            ],
        ]

        for (const [detail, labels] of cases) {
            vi.mocked(getKnowledgeArticleAPI).mockResolvedValueOnce({
                ...detail,
                content: '<em>虚构正文</em>\n第二行',
            })
            const { view } = render()
            await open(view)
            await flushPromises()

            expect(view.get('.article-detail [role="status"]').text()).toBe(
                `本次查询返回：${labels.join('；')}`,
            )
            expect(view.get('.article-content').element.textContent).toBe(
                '<em>虚构正文</em>\n第二行',
            )
            expect(view.find('.article-content em').exists()).toBe(false)
            expect(view.text()).toContain('响应接收时间')
            expect(view.text()).toContain('状态可能随后变化')
            expect(view.text()).toContain('原文未编号')
            expect(view.get('.article-detail').attributes('aria-busy')).toBe('false')

            view.unmount()
            wrapper = undefined
        }
    })

    it('requery and recovery', async () => {
        vi.mocked(getKnowledgeArticleAPI).mockResolvedValueOnce(article())
        const { view } = render()
        await open(view)
        await flushPromises()
        expect(view.text()).toContain('查询时为当前有效版本')

        const pending = deferred<KnowledgeArticleDetail>()
        vi.mocked(getKnowledgeArticleAPI).mockReturnValueOnce(pending.promise)
        await button(view, '重新查询').trigger('click')

        expect(view.text()).toContain('正在查询；当前状态未核实')
        expect(view.text()).not.toContain('查询时为当前有效版本')
        expect(view.find('.article-content').exists()).toBe(false)
        expect(button(view, '重新查询').element.disabled).toBe(true)

        pending.reject(new Error('test-only failure'))
        await flushPromises()

        expect(view.get('[role="alert"]').text()).toContain(
            '读取历史条款失败。当前状态未核实',
        )
        expect(button(view, '重新查询').element.disabled).toBe(false)
        expect(getKnowledgeArticleAPI).toHaveBeenCalledTimes(2)

        vi.mocked(getKnowledgeArticleAPI).mockResolvedValueOnce(article({
            is_current: false,
            is_current_effective: false,
            is_expired: true,
        }))
        await button(view, '重新查询').trigger('click')
        await flushPromises()

        expect(view.find('[role="alert"]').exists()).toBe(false)
        expect(view.text()).toContain('已过期')
        expect(view.text()).not.toContain('查询时为当前有效版本')
        expect(getKnowledgeArticleAPI).toHaveBeenCalledTimes(3)
    })

    it('switching references', async () => {
        const oldRequest = deferred<KnowledgeArticleDetail>()
        const newRequest = deferred<KnowledgeArticleDetail>()
        vi.mocked(getKnowledgeArticleAPI)
            .mockReturnValueOnce(oldRequest.promise)
            .mockReturnValueOnce(newRequest.promise)

        const { view } = render([citation(), citation(2)])
        await open(view, 0)
        const oldSignal = lastSignal()

        await open(view, 1)
        const newSignal = lastSignal()
        expect(oldSignal.aborted).toBe(true)
        expect(newSignal.aborted).toBe(false)

        oldRequest.resolve(article({}, '旧查询正文'))
        await flushPromises()

        expect(view.get('.article-detail').text()).toContain('[R2] 条款详情')
        expect(view.get('.article-detail').attributes('aria-busy')).toBe('true')
        expect(view.text()).not.toContain('旧查询正文')

        newRequest.resolve(article({}, '新查询正文'))
        await flushPromises()

        expect(view.get('.article-content').text()).toBe('新查询正文')
        expect(view.get('.article-detail').attributes('aria-busy')).toBe('false')
        expect(getKnowledgeArticleAPI).toHaveBeenCalledTimes(2)
    })

    it('lifecycle cancellation', async () => {
        for (const mode of ['close', 'replace', 'mutate', 'unmount', 'auth']) {
            for (const outcome of ['success', '401']) {
                const pending = deferred<KnowledgeArticleDetail>()
                vi.mocked(getKnowledgeArticleAPI).mockReturnValueOnce(pending.promise)
                const { view, auth } = render()
                await open(view)
                const signal = lastSignal()

                if (mode === 'close') {
                    await button(view, '关闭').trigger('click')
                } else if (mode === 'replace') {
                    await view.setProps({ items: [citation(2)] })
                } else if (mode === 'mutate') {
                    view.props('items')[0]!.title = '修改后的引用快照'
                    await nextTick()
                } else if (mode === 'unmount') {
                    view.unmount()
                    wrapper = undefined
                } else {
                    auth.logout()
                    auth.token = 'new-session'
                    await nextTick()
                }

                expect(signal.aborted, `${mode}/${outcome}`).toBe(true)

                if (outcome === 'success') {
                    pending.resolve(article({}, '不应显示的旧结果'))
                } else {
                    pending.reject(new ChatStreamRequestError(
                        401, 'unauthorized', 'test-only stale 401',
                    ))
                }
                await flushPromises()

                expect(view.emitted('unauthorized')).toBeUndefined()
                if (mode !== 'unmount') {
                    expect(view.find('.article-detail').exists()).toBe(false)
                    expect(view.text()).not.toContain('不应显示的旧结果')
                    view.unmount()
                    wrapper = undefined
                }
                if (mode === 'auth') {
                    expect(auth.token).toBe('new-session')
                }
            }
        }
    })

    it('current unauthorized', async () => {
        const error = new ChatStreamRequestError(
            401, 'unauthorized', 'test-only current 401',
        )
        vi.mocked(getKnowledgeArticleAPI).mockRejectedValueOnce(error)

        const { view } = render()
        await open(view)
        await flushPromises()

        expect(view.emitted('unauthorized')).toEqual([[error]])
        expect(view.find('.article-detail').exists()).toBe(false)
        expect(view.text()).toContain('当前状态未核实')
        expect(getKnowledgeArticleAPI).toHaveBeenCalledTimes(1)
    })

    it('canceled request', async () => {
        vi.mocked(getKnowledgeArticleAPI).mockRejectedValueOnce(
            new DOMException('test-only cancellation', 'AbortError'),
        )
        const { view } = render()
        await open(view)
        await flushPromises()

        expect(view.emitted('unauthorized')).toBeUndefined()
        expect(view.find('[role="alert"]').exists()).toBe(false)
        expect(view.text()).toContain('当前状态未核实')
        expect(view.get('.article-detail').attributes('aria-busy')).toBe('false')
    })
})