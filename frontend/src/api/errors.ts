import axios from 'axios'

export function isCanceled(error: unknown): boolean {
    return axios.isCancel(error)
}

export function isUnauthorized(error: unknown): boolean {
    return axios.isAxiosError(error) && error.response?.status === 401
}

export function errorMessage(
    error: unknown,
    fallback: string,
): string {
    if (!axios.isAxiosError(error)) {
        return fallback
    }

    const status = error.response?.status

    if (status === 400) {
        return '提交内容不符合要求，请检查后重试'
    }

    if (status === 401) {
        return '登录凭证无效或已过期'
    }

    if (status === 403) {
        return '没有权限执行此操作'
    }

    if (status === 404) {
        return '请求的资源不存在或不可访问'
    }

    if (status === 413) {
        return '提交内容过大，请缩短后重试'
    }

    if (status === 429) {
        const retryAfter = Number(error.response?.headers['retry-after'])

        return Number.isFinite(retryAfter) && retryAfter > 0
            ? `请求过于频繁，请在 ${Math.ceil(retryAfter)} 秒后重试`
            : '请求过于频繁，请稍后重试'
    }

    if (status === 502 || status === 503 || status === 504) {
        return '服务暂时不可用或处理超时，请稍后重试'
    }

    if (!error.response) {
        return '网络连接失败或请求超时，请检查服务'
    }

    return fallback
}