// @vitest-environment node
import { afterEach, describe, expect, it, vi } from 'vitest'
import { decodeActionRequired, decodeAgentResult } from './agent-protocol'
import { streamAgentMessageAPI } from './chat-stream'
import { applyAgentEvent, canConfirm, createAgentState, finishAgentStream } from '../chat/agent-state'
import { actionRequired, agentDone, agentResult, runID } from '../test/agent-fixtures'

vi.mock('../auth/token', () => ({ readToken: () => 'fixture-bearer' }))
afterEach(() => vi.unstubAllGlobals())

// Real HTTP proposal evidence and the live browser both use policy_unavailable.
// This field comes from NormalizeAppealSnapshot, not the Agent audit-code enum.
describe('confirmation eligibility reason contract', () => {
    it.each([
        'policy_unavailable', 'missing_fields', 'policy_scope_mismatch',
        'policy_evidence_unavailable', 'facts_unverified',
        'policy_specific_reason', 'a', 'a'.repeat(64),
    ])('accepts backend-valid eligibility reason %s', (reason) => {
        const decoded = decodeActionRequired({ ...actionRequired(), reason_code: reason })
        expect(decoded.reason_code).toBe(reason)
        expect(decoded.eligibility).toBe('manual_review')
    })

    it.each([
        '', 'Policy_unavailable', 'policy-unavailable', ' reason', 'reason ',
        'reason\n', 'reason\r\n', '政策', 'a'.repeat(65), null, 1,
    ])('rejects malformed eligibility reason %j', (reason) => {
        expect(() => decodeActionRequired({ ...actionRequired(), reason_code: reason })).toThrow()
    })

    it('keeps Agent result audit codes strict', () => {
        expect(() => decodeAgentResult(agentResult({ reason_code: 'policy_unavailable' }))).toThrow()
    })

    it('finishes a real-shape manual-review proposal and enables confirmation', async () => {
        const frames: [string, unknown][] = [
            ['start', { conversation_id: 1 }],
            ['agent_start', { run_id: runID }],
            ['action_required', { ...actionRequired(), reason_code: 'policy_unavailable' }],
            ['agent_result', agentResult({ decision: 'confirmation_required', reason_code: 'confirmation_required' })],
            ['token', { content: '请核对本地人工复核申请。' }],
            ['citation', { items: [] }],
            ['done', { ...agentDone(), agent: agentResult({
                decision: 'confirmation_required', reason_code: 'confirmation_required', message_save_status: 'saved',
            }) }],
        ]
        const wire = frames.map(([event, data]) => `event: ${event}\ndata: ${JSON.stringify(data)}\n\n`).join('')
        vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response(wire, {
            headers: { 'Content-Type': 'text/event-stream; charset=utf-8' },
        })))
        const state = createAgentState()
        const seen: string[] = []
        await streamAgentMessageAPI(1, { content: '请准备人工复核卡片。' }, {
            signal: new AbortController().signal,
            onEvent(event) {
                seen.push(event.event)
                applyAgentEvent(state, event)
            },
        })
        finishAgentStream(state)
        expect(seen).toContain('action_required')
        expect(seen.at(-1)).toBe('done')
        expect(state.completed).toBe(true)
        expect(state.messageSaveStatus).toBe('saved')
        expect(state.operation?.proposal.reason_code).toBe('policy_unavailable')
        expect(canConfirm(state, Date.parse(actionRequired().issued_at) + 1000)).toBe(true)
    })
})
