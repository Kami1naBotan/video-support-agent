package handler

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestWriteSSEEventPreservesContent(t *testing.T) {
	var output bytes.Buffer

	err := writeSSEEvent(
		&output,
		SSEEventToken,
		struct {
			Content string `json:"content"`
		}{
			Content: "第一段\n第二段",
		},
	)
	if err != nil {
		t.Fatalf("write event: %v", err)
	}

	want := "event: token\n" +
		`data: {"content":"第一段\n第二段"}` +
		"\n\n"

	if output.String() != want {
		t.Fatalf("expected %q, got %q", want, output.String())
	}

	if strings.Count(output.String(), "\n\n") != 1 {
		t.Fatal("expected exactly one SSE event boundary")
	}
}

type failingSSEWriter struct {
	err error
}

func (w failingSSEWriter) Write([]byte) (int, error) {
	return 0, w.err
}

func TestWriteSSEEventReturnsWriteError(t *testing.T) {
	writeErr := errors.New("connection closed")

	err := writeSSEEvent(
		failingSSEWriter{err: writeErr},
		SSEEventToken,
		map[string]string{"content": "你好"},
	)

	if !errors.Is(err, writeErr) {
		t.Fatalf("expected original write error, got %v", err)
	}
}
