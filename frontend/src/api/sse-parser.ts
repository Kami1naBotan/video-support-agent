export interface SSEFrame {
    event: string
    data: string
}

export function createSSEParser(
    onFrame: (frame: SSEFrame) => void,
) {
    let buffer = ''
    let eventName = ''
    let dataLines: string[] = []

    function processLine(line: string): void {
        // 空行表示当前事件结束。
        if (line === '') {
            const frame = dataLines.length > 0
                ? {
                    event: eventName || 'message',
                    data: dataLines.join('\n'),
                }
                : null

            eventName = ''
            dataLines = []

            if (frame !== null) {
                onFrame(frame)
            }
            return
        }

        // SSE 注释行，例如心跳。
        if (line.startsWith(':')) {
            return
        }

        const colonIndex = line.indexOf(':')
        const field = colonIndex === -1
            ? line
            : line.slice(0, colonIndex)

        let value = colonIndex === -1
            ? ''
            : line.slice(colonIndex + 1)

        // 按协议只去掉冒号后的一个空格。
        if (value.startsWith(' ')) {
            value = value.slice(1)
        }

        if (field === 'event') {
            eventName = value
        } else if (field === 'data') {
            dataLines.push(value)
        }
    }

    function push(text: string): void {
        buffer += text

        while (true) {
            const newlineIndex = buffer.indexOf('\n')
            if (newlineIndex === -1) {
                break
            }

            let line = buffer.slice(0, newlineIndex)
            buffer = buffer.slice(newlineIndex + 1)

            // 同时兼容 LF 和 CRLF 换行。
            if (line.endsWith('\r')) {
                line = line.slice(0, -1)
            }

            processLine(line)
        }
    }

    function finish(): void {
        if (
            buffer.length > 0 ||
            eventName !== '' ||
            dataLines.length > 0
        ) {
            throw new Error('SSE 响应在事件结束前中断')
        }
    }

    return { push, finish }
}