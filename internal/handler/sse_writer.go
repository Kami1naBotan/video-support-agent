package handler

import (
	"encoding/json"
	"fmt"
	"io"
)

func writeSSEEvent(w io.Writer, event SSEEvent, data any) error {
	switch event {
	case SSEEventStart, SSEEventToken, SSEEventCitation, SSEEventError, SSEEventDone:
	default:
		return fmt.Errorf("unsupported SSE event: %q", event)
	}

	payload, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("encode SSE data: %w", err)
	}

	frame := []byte(fmt.Sprintf("event: %s\ndata: %s\n\n", event, payload))

	n, err := w.Write(frame)
	if err != nil {
		return fmt.Errorf("write SSE event: %w", err)
	}
	if n != len(frame) {
		return io.ErrShortWrite
	}

	return nil
}
