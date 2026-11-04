import {
    objectValue,
    positiveID,
    uuidValue,
} from '../api/agent-protocol'

const PREFIX = 'agent-recovery:v1:'

export interface RecoveryCheckpoint {
    version: 1
    user_id: number
    conversation_id: number
    run_id?: string
    operation_key?: string
}

function storageKey(userID: number, conversationID: number): string {
    positiveID(userID)
    positiveID(conversationID)
    return `${PREFIX}${userID}:${conversationID}`
}

function decodeCheckpoint(
    value: unknown,
    userID: number,
    conversationID: number,
): RecoveryCheckpoint {
    const data = objectValue(value)
    const allowed = [
        'version',
        'user_id',
        'conversation_id',
        'run_id',
        'operation_key',
    ]

    if (
        Object.keys(data).some((key) => !allowed.includes(key)) ||
        data.version !== 1 ||
        positiveID(data.user_id) !== userID ||
        positiveID(data.conversation_id) !== conversationID
    ) {
        throw new Error('恢复定位记录无效')
    }

    const result: RecoveryCheckpoint = {
        version: 1,
        user_id: userID,
        conversation_id: conversationID,
    }

    if (data.run_id !== undefined) {
        result.run_id = uuidValue(data.run_id)
    }
    if (data.operation_key !== undefined) {
        result.operation_key = uuidValue(data.operation_key)
    }

    return result
}

export function readRecoveryCheckpoint(
    userID: number,
    conversationID: number,
): RecoveryCheckpoint | undefined {
    try {
        const raw = sessionStorage.getItem(
            storageKey(userID, conversationID),
        )
        if (raw === null) return undefined

        const value: unknown = JSON.parse(raw)
        return decodeCheckpoint(value, userID, conversationID)
    } catch {
        // 损坏的记录不能被当作“从未发起请求”。
        throw new Error('无法读取恢复记录，请先核对已有执行结果')
    }
}

export function writeRecoveryCheckpoint(
    userID: number,
    conversationID: number,
    runID?: string,
    operationKey?: string,
): void {
    const record: RecoveryCheckpoint = {
        version: 1,
        user_id: positiveID(userID),
        conversation_id: positiveID(conversationID),
    }

    if (runID !== undefined) {
        record.run_id = uuidValue(runID)
    }
    if (operationKey !== undefined) {
        record.operation_key = uuidValue(operationKey)
    }

    try {
        sessionStorage.setItem(
            storageKey(userID, conversationID),
            JSON.stringify(record),
        )
    } catch {
        throw new Error('无法保存恢复记录，本次请求不能安全继续')
    }
}

export function clearRecoveryCheckpoints(): void {
    try {
        const keys: string[] = []

        for (let index = 0; index < sessionStorage.length; index++) {
            const key = sessionStorage.key(index)
            if (key?.startsWith(PREFIX)) keys.push(key)
        }

        for (const key of keys) {
            sessionStorage.removeItem(key)
        }
    } catch {
        // 清理失败不能阻止退出登录。
        // 剩余记录只含定位信息，读取仍按用户和会话隔离。
    }
}