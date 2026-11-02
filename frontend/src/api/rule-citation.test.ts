import { describe, expect, it } from 'vitest'

import {
    decodeRuleCitations,
    isDecimalID,
    officialSourceHref,
} from './rule-citation'
import { decodeChatStreamEvent } from './chat-stream-event'
import { citation } from '../test/rag-fixtures'

describe('rule citations', () => {
    it('decimal IDs', () => {
        for (const value of [
            '1',
            '9007199254740993',
            '18446744073709551615',
        ]) {
            expect(isDecimalID(value), value).toBe(true)
        }

        for (const value of [
            undefined, null, 1, 9007199254740992,
            '', '0', '01', '-1', '+1', '1.0', '1e3', ' 1',
            '18446744073709551616', '100000000000000000000',
        ]) {
            expect(isDecimalID(value), String(value)).toBe(false)
        }
    })

    it('official sources', () => {
        expect(
            officialSourceHref(' https://www.bilibili.com:443/test-only-rule '),
        ).toBe('https://www.bilibili.com/test-only-rule')

        expect(
            officialSourceHref('http://www.bilibili.com/test-only-rule'),
        ).toBe('http://www.bilibili.com/test-only-rule')

        for (const value of [
            '',
            '/test-only-rule',
            'https://example.test/rule',
            'https://bilibili.com/rule',
            'https://www.bilibili.com:8443/rule',
            'ftp://www.bilibili.com/rule',
        ]) {
            expect(officialSourceHref(value), value).toBeNull()
        }
    })

    it('snapshots and copies', () => {
        expect(decodeRuleCitations([])).toEqual([])

        const input = citation(2)
        input.expires_at = '2026-10-01T00:00:00Z'

        const result = decodeRuleCitations([input])
        expect(result).toEqual([input])
        expect(result[0]).not.toBe(input)
        expect(result[0]!.chunk_ids).not.toBe(input.chunk_ids)

        // R2 可以单独出现；已过期快照也不会被解码器删除。
        expect(result[0]!.ref_id).toBe('R2')
        expect(result[0]!.document_id).toBe('9007199254740993')
        expect(result[0]!.category_id).toBe('18446744073709551615')

        input.title = '修改输入'
        input.chunk_ids.push('999')
        expect(result[0]!.title).toBe('虚构测试规则2')
        expect(result[0]!.chunk_ids).toEqual(['102'])

        result[0]!.chunk_ids.push('998')
        expect(input.chunk_ids).toEqual(['102', '999'])
    })

    it('malformed citations', () => {
        const invalid: unknown[] = [
            null, undefined, {}, '[]', [null], [[]],
            [{ ...citation(), ref_id: 'R0' }],
            [{ ...citation(), ref_id: 'R6' }],
            [{ ...citation(), title: ' ' }],
            [{ ...citation(), version: '' }],
            [{ ...citation(), section_path: null }],
            [{ ...citation(), article_number: 1 }],
            [{ ...citation(), source_url: 'https://example.test/rule' }],
            [{ ...citation(), effective_at: '2026-01-01' }],
            [{ ...citation(), retrieved_at: 'not-a-time' }],
            [{ ...citation(), expires_at: '' }],
            [{ ...citation(), chunk_ids: [] }],
            [{ ...citation(), chunk_ids: ['999'] }],
            [{ ...citation(), chunk_ids: ['101', 102] }],
        ]

        for (const value of invalid) {
            expect(() => decodeRuleCitations(value)).toThrow(
                '规则引用数据格式无效',
            )
        }

        // 每个字段均为必需；expires_at 允许 null，但不允许缺失。
        for (const key of Object.keys(citation())) {
            const raw: Record<string, unknown> = { ...citation() }
            delete raw[key]
            expect(() => decodeRuleCitations([raw]), key).toThrow()
        }

        for (const key of [
            'document_id', 'version_id', 'article_id', 'category_id', 'chunk_id',
        ]) {
            expect(() => decodeRuleCitations([
                { ...citation(), [key]: 1 },
            ]), key).toThrow()
        }
    })

    it('uniqueness and limits', () => {
        const five = Array.from({ length: 5 }, (_, index) => citation(index + 1))
        expect(decodeRuleCitations(five)).toEqual(five)
        expect(() => decodeRuleCitations([...five, citation()])).toThrow()

        const first = citation()
        expect(() => decodeRuleCitations([
            first, { ...citation(2), ref_id: first.ref_id },
        ])).toThrow()

        expect(() => decodeRuleCitations([
            first, { ...citation(2), article_id: first.article_id },
        ])).toThrow()

        expect(() => decodeRuleCitations([
            first,
            { ...citation(2), chunk_id: first.chunk_id, chunk_ids: [first.chunk_id] },
        ])).toThrow()

        expect(() => decodeRuleCitations([
            { ...first, chunk_ids: ['101', '101'] },
        ])).toThrow()

        const atLimit = {
            ...first,
            chunk_id: '1',
            chunk_ids: Array.from({ length: 250 }, (_, index) => String(index + 1)),
        }
        expect(decodeRuleCitations([atLimit])[0]!.chunk_ids).toHaveLength(250)

        expect(() => decodeRuleCitations([
            { ...atLimit, chunk_ids: [...atLimit.chunk_ids, '251'] },
        ])).toThrow()

        // 每个引用自身合法，合计超过 250 时仍必须拒绝。
        expect(() => decodeRuleCitations([
            atLimit,
            { ...citation(2), chunk_id: '251', chunk_ids: ['251'] },
        ])).toThrow()
    })

    it('SSE citation decoding', () => {
        const items = [citation(2)]
        expect(decodeChatStreamEvent({
            event: 'citation',
            data: JSON.stringify({ items }),
        })).toEqual({ event: 'citation', data: { items } })

        for (const invalid of [
            null,
            [{ ...citation(), document_id: 1 }],
            [{ ...citation(), chunk_ids: [] }],
            [citation(), citation()],
        ]) {
            expect(() => decodeRuleCitations(invalid)).toThrow()
            expect(() => decodeChatStreamEvent({
                event: 'citation',
                data: JSON.stringify({ items: invalid }),
            })).toThrow()
        }
    })
})