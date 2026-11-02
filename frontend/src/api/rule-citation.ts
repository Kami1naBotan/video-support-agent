export interface RuleCitation {
    ref_id: string
    document_id: string
    version_id: string
    article_id: string
    category_id: string
    chunk_id: string
    chunk_ids: string[]
    version: string
    title: string
    section_path: string
    article_number: string
    source_url: string
    effective_at: string
    expires_at: string | null
    retrieved_at: string
}

export function isRecord(
    value: unknown,
): value is Record<string, unknown> {
    return (
        typeof value === 'object' &&
        value !== null &&
        !Array.isArray(value)
    )
}

export function isDecimalID(value: unknown): value is string {
    if (
        typeof value !== 'string' ||
        !/^[1-9][0-9]{0,19}$/.test(value)
    ) {
        return false
    }

    // 同长度的规范十进制字符串可以直接按字典序比较。
    return (
        value.length < 20 ||
        value <= '18446744073709551615'
    )
}

export function officialSourceHref(raw: string): string | null {
    try {
        const url = new URL(raw.trim())

        if (
            (url.protocol !== 'https:' && url.protocol !== 'http:') ||
            url.hostname !== 'www.bilibili.com' ||
            url.username !== '' ||
            url.password !== '' ||
            url.port !== ''
        ) {
            return null
        }

        return url.href
    } catch {
        return null
    }
}

function invalidCitation(): never {
    throw new Error('规则引用数据格式无效')
}

function readString(
    item: Record<string, unknown>,
    key: string,
    allowEmpty = false,
): string {
    const value = item[key]

    if (
        typeof value !== 'string' ||
        (!allowEmpty && value.trim() === '')
    ) {
        invalidCitation()
    }

    return value
}

function readID(
    item: Record<string, unknown>,
    key: string,
): string {
    const value = item[key]
    if (!isDecimalID(value)) invalidCitation()
    return value
}

function readTimestamp(value: unknown): string {
    if (
        typeof value !== 'string' ||
        !/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$/.test(value) ||
        !Number.isFinite(Date.parse(value))
    ) {
        invalidCitation()
    }

    return value
}

export function decodeRuleCitations(value: unknown): RuleCitation[] {
    if (!Array.isArray(value) || value.length > 5) {
        invalidCitation()
    }

    const references = new Set<string>()
    const articles = new Set<string>()
    const chunks = new Set<string>()

    return value.map((raw: unknown): RuleCitation => {
        if (!isRecord(raw)) invalidCitation()

        const refID = readString(raw, 'ref_id')
        if (!/^R[1-5]$/.test(refID) || references.has(refID)) {
            invalidCitation()
        }
        references.add(refID)

        const documentID = readID(raw, 'document_id')
        const versionID = readID(raw, 'version_id')
        const articleID = readID(raw, 'article_id')
        const categoryID = readID(raw, 'category_id')
        const chunkID = readID(raw, 'chunk_id')

        const articleKey = `${documentID}/${versionID}/${articleID}`
        if (articles.has(articleKey)) invalidCitation()
        articles.add(articleKey)

        const rawChunks = raw.chunk_ids
        if (
            !Array.isArray(rawChunks) ||
            rawChunks.length === 0 ||
            rawChunks.length > 250 ||
            !rawChunks.every(isDecimalID) ||
            !rawChunks.includes(chunkID)
        ) {
            invalidCitation()
        }

        const chunkIDs: string[] = [...rawChunks]
        for (const id of chunkIDs) {
            if (chunks.has(id)) invalidCitation()
            chunks.add(id)
            if (chunks.size > 250) invalidCitation()
        }

        const sourceURL = readString(raw, 'source_url')
        if (officialSourceHref(sourceURL) === null) {
            invalidCitation()
        }

        return {
            ref_id: refID,
            document_id: documentID,
            version_id: versionID,
            article_id: articleID,
            category_id: categoryID,
            chunk_id: chunkID,
            chunk_ids: chunkIDs,
            version: readString(raw, 'version'),
            title: readString(raw, 'title'),
            section_path: readString(raw, 'section_path', true),
            article_number: readString(raw, 'article_number', true),
            source_url: sourceURL,
            effective_at: readTimestamp(raw.effective_at),
            expires_at:
                raw.expires_at === null
                    ? null
                    : readTimestamp(raw.expires_at),
            retrieved_at: readTimestamp(raw.retrieved_at),
        }
    })
}