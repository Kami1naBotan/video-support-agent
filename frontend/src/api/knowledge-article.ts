import { http } from './http'
import { isDecimalID, isRecord } from './rule-citation'
import type { RuleCitation } from './rule-citation'

export interface KnowledgeArticleState {
    is_current: boolean
    is_superseded: boolean
    is_expired: boolean
    is_withdrawn: boolean
    is_current_effective: boolean
}

export interface KnowledgeArticleDetail {
    section_path: string
    article_number: string
    content: string
    state: KnowledgeArticleState
}

function invalidDetail(): never {
    throw new Error('历史条款响应格式或身份不匹配')
}

export async function getKnowledgeArticleAPI(
    citation: RuleCitation,
    signal: AbortSignal,
): Promise<KnowledgeArticleDetail> {
    signal.throwIfAborted()

    if (
        !isDecimalID(citation.document_id) ||
        !isDecimalID(citation.version_id) ||
        !isDecimalID(citation.article_id) ||
        !isDecimalID(citation.category_id)
    ) {
        invalidDetail()
    }

    const versionID = encodeURIComponent(citation.version_id)
    const articleID = encodeURIComponent(citation.article_id)

    const response = await http.get<unknown>(
        `/knowledge/versions/${versionID}/articles/${articleID}`,
        {
            signal,
            headers: { 'Cache-Control': 'no-cache' },
        },
    )

    signal.throwIfAborted()

    const body = response.data
    if (
        !isRecord(body) ||
        !isRecord(body.identity) ||
        !isRecord(body.article) ||
        !isRecord(body.state)
    ) {
        invalidDetail()
    }

    const identity = body.identity
    if (
        identity.document_id !== citation.document_id ||
        identity.version_id !== citation.version_id ||
        identity.article_id !== citation.article_id ||
        identity.category_id !== citation.category_id
    ) {
        invalidDetail()
    }

    const article = body.article
    if (
        typeof article.section_path !== 'string' ||
        typeof article.article_number !== 'string' ||
        typeof article.content !== 'string'
    ) {
        invalidDetail()
    }

    const rawState = body.state
    if (
        typeof rawState.is_current !== 'boolean' ||
        typeof rawState.is_superseded !== 'boolean' ||
        typeof rawState.is_expired !== 'boolean' ||
        typeof rawState.is_withdrawn !== 'boolean' ||
        typeof rawState.is_current_effective !== 'boolean'
    ) {
        invalidDetail()
    }

    const state: KnowledgeArticleState = {
        is_current: rawState.is_current,
        is_superseded: rawState.is_superseded,
        is_expired: rawState.is_expired,
        is_withdrawn: rawState.is_withdrawn,
        is_current_effective: rawState.is_current_effective,
    }

    if (
        state.is_current_effective &&
        (
            !state.is_current ||
            state.is_superseded ||
            state.is_expired ||
            state.is_withdrawn
        )
    ) {
        invalidDetail()
    }

    return {
        section_path: article.section_path,
        article_number: article.article_number,
        content: article.content,
        state,
    }
}