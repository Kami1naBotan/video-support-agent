import type { RuleCitation } from '../api/rule-citation'
import type {
    KnowledgeArticleDetail,
    KnowledgeArticleState,
} from '../api/knowledge-article'
import type { ChatMessage } from '../api/message'

// 全部为虚构测试数据，不作为真实规则依据，不访问来源 URL。
export function citation(n = 1): RuleCitation {
    return {
        ref_id: `R${n}`,
        document_id: '9007199254740993',
        version_id: '9007199254740995',
        article_id: `900719925474100${n}`,
        category_id: '18446744073709551615',
        chunk_id: String(100 + n),
        chunk_ids: [String(100 + n)],
        version: '虚构测试版本',
        title: `虚构测试规则${n}`,
        section_path: '测试章节',
        article_number: '',
        source_url: 'https://www.bilibili.com/test-only-rule',
        effective_at: '2026-01-01T00:00:00Z',
        expires_at: null,
        retrieved_at: '2026-09-01T00:00:00Z',
    }
}

export function article(
    state: Partial<KnowledgeArticleState> = {},
    content = '虚构条款正文',
): KnowledgeArticleDetail {
    return {
        section_path: '测试章节',
        article_number: '',
        content,
        state: {
            is_current: true,
            is_superseded: false,
            is_expired: false,
            is_withdrawn: false,
            is_current_effective: true,
            ...state,
        },
    }
}

export function message(
    overrides: Partial<ChatMessage> = {},
): ChatMessage {
    return {
        id: 11,
        conversation_id: 1,
        role: 'assistant',
        content: '虚构回答 [R1]',
        created_at: '2026-09-01T00:00:01Z',
        citations: [],
        ...overrides,
    }
}

export function deferred<T>() {
    let resolve!: (value: T) => void
    let reject!: (reason?: unknown) => void

    const promise = new Promise<T>((resolvePromise, rejectPromise) => {
        resolve = resolvePromise
        reject = rejectPromise
    })

    return { promise, resolve, reject }
}