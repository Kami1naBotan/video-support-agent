import { afterEach, describe, expect, it, vi } from 'vitest'
import { nextTick, reactive } from 'vue'
import { mount } from '@vue/test-utils'
import type { VueWrapper } from '@vue/test-utils'
import ElementPlus from 'element-plus'
import AgentPanel from './AgentPanel.vue'
import { readyAgentState } from '../test/agent-ui-fixtures'

let wrapper: VueWrapper | undefined

afterEach(() => {
    wrapper?.unmount()
    wrapper = undefined
    vi.restoreAllMocks()
    vi.useRealTimers()
})

function confirmButton(page: VueWrapper) {
    const button = page.findAll<HTMLButtonElement>('button')
        .find((item) => item.text() === '确认提交人工复核')
    if (!button) throw new Error('missing confirmation button')
    return button
}

describe('AgentPanel interaction', () => {
    it('requires acknowledgement and clears it on phase change', async () => {
        const state = reactive(readyAgentState())
        const page = mount(AgentPanel, {
            props: { state, disabled: false },
            global: { plugins: [ElementPlus] },
        })
        wrapper = page

        expect(confirmButton(page).element.disabled).toBe(true)
        expect(page.text()).toContain('不代表平台已受理或申诉通过')
        expect(page.text()).not.toContain('fixture-only-token')

        await page.get('input[type="checkbox"]').setValue(true)
        expect(confirmButton(page).element.disabled).toBe(false)
        await confirmButton(page).trigger('click')
        expect(page.emitted('confirm')).toHaveLength(1)

        state.operation!.phase = 'submitting'
        await nextTick()
        expect(page.find('input[type="checkbox"]').exists()).toBe(false)

        state.operation!.phase = 'ready'
        await nextTick()
        expect(page.get<HTMLInputElement>('input[type="checkbox"]').element.checked)
            .toBe(false)
        expect(confirmButton(page).element.disabled).toBe(true)
    })

    it('expires confirmation and releases timer', async () => {
        vi.useFakeTimers()
        const clear = vi.spyOn(globalThis, 'clearInterval')
        const state = readyAgentState()
        state.operation!.proposal.expires_at =
            new Date(Date.now() + 500).toISOString()

        const page = mount(AgentPanel, {
            props: { state, disabled: false },
            global: { plugins: [ElementPlus] },
        })
        wrapper = page
        await page.get('input[type="checkbox"]').setValue(true)
        expect(confirmButton(page).element.disabled).toBe(false)

        await vi.advanceTimersByTimeAsync(1000)
        await nextTick()
        expect(confirmButton(page).element.disabled).toBe(true)
        expect(page.text()).toContain('确认卡片已过期')

        page.unmount()
        wrapper = undefined
        expect(clear).toHaveBeenCalled()
    })
})