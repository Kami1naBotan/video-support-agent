import { beforeEach, describe, expect, it, vi } from 'vitest'

import { http } from './http'
import { getKnowledgeArticleAPI } from './knowledge-article'
import { article, citation, deferred } from '../test/rag-fixtures'

vi.mock('./http', () => ({
    http: { get: vi.fn() },
}))

function response(): {
    identity: Record<string, unknown>
    article: Record<string, unknown>
    state: Record<string, unknown>
} {
    const item = citation()
    const detail = article()

    return {
        identity: {
            document_id: item.document_id,
            version_id: item.version_id,
            article_id: item.article_id,
            category_id: item.category_id,
        },
        article: {
            id: 1,
            version_id: 2,
            section_path: detail.section_path,
            article_number: detail.article_number,
            content: detail.content,
        },
        state: { ...detail.state },
    }
}

beforeEach(() => {
    vi.resetAllMocks()
})

describe('knowledge article API', () => {
    it('exact identity and request', async () => {
        const item = citation()
        const body = response()
        const controller = new AbortController()
        vi.mocked(http.get).mockResolvedValue({ data: body })

        const result = await getKnowledgeArticleAPI(item, controller.signal)

        expect(http.get).toHaveBeenCalledExactlyOnceWith(
            `/knowledge/versions/${item.version_id}/articles/${item.article_id}`,
            {
                signal: controller.signal,
                headers: { 'Cache-Control': 'no-cache' },
            },
        )
        expect(result).toEqual(article())

        // 返回的状态是挑选出的副本，不保留响应对象引用。
        body.state.is_current = false
        expect(result.state.is_current).toBe(true)
    })

    it('malformed detail', async () => {
        const signal = new AbortController().signal
        const bad: unknown[] = [null, [], {}]

        for (const group of ['identity', 'article', 'state'] as const) {
            bad.push({ ...response(), [group]: null })
            bad.push({ ...response(), [group]: [] })
        }

        for (const key of [
            'document_id', 'version_id', 'article_id', 'category_id',
        ]) {
            for (const value of [undefined, 1, '999']) {
                const body = response()
                body.identity[key] = value
                bad.push(body)
            }
        }

        for (const key of ['section_path', 'article_number', 'content']) {
            const body = response()
            body.article[key] = null
            bad.push(body)
        }

        for (const key of Object.keys(article().state)) {
            for (const value of [undefined, 1, 'true']) {
                const body = response()
                body.state[key] = value
                bad.push(body)
            }
        }

        for (const change of [
            { is_current: false },
            { is_superseded: true },
            { is_expired: true },
            { is_withdrawn: true },
        ]) {
            const body = response()
            Object.assign(body.state, change)
            bad.push(body)
        }

        for (const body of bad) {
            vi.mocked(http.get).mockResolvedValueOnce({ data: body })
            await expect(getKnowledgeArticleAPI(citation(), signal)).rejects.toThrow(
                '历史条款响应格式或身份不匹配',
            )
        }

        // 未标记当前有效的历史状态可以正常读回。
        const historical = response()
        Object.assign(historical.state, {
            is_current: false,
            is_superseded: true,
            is_expired: true,
            is_withdrawn: true,
            is_current_effective: false,
        })
        vi.mocked(http.get).mockResolvedValueOnce({ data: historical })
        expect(
            (await getKnowledgeArticleAPI(citation(), signal)).state,
        ).toEqual(historical.state)

        vi.mocked(http.get).mockClear()
        await expect(getKnowledgeArticleAPI(
            { ...citation(), version_id: '0' },
            signal,
        )).rejects.toThrow()
        expect(http.get).not.toHaveBeenCalled()
    })

    it('cancellation', async () => {
        const early = new AbortController()
        early.abort()
        await expect(
            getKnowledgeArticleAPI(citation(), early.signal),
        ).rejects.toMatchObject({ name: 'AbortError' })
        expect(http.get).not.toHaveBeenCalled()

        const pending = deferred<{ data: ReturnType<typeof response> }>()
        const late = new AbortController()
        vi.mocked(http.get).mockReturnValueOnce(pending.promise)

        const request = getKnowledgeArticleAPI(citation(), late.signal)
        late.abort()
        pending.resolve({ data: response() })

        await expect(request).rejects.toMatchObject({ name: 'AbortError' })
        expect(http.get).toHaveBeenCalledTimes(1)
    })

    it('request failures', async () => {
        const error = new Error('test-only transport failure')
        vi.mocked(http.get).mockRejectedValueOnce(error)

        await expect(getKnowledgeArticleAPI(
            citation(),
            new AbortController().signal,
        )).rejects.toBe(error)
        expect(http.get).toHaveBeenCalledTimes(1)
    })
})