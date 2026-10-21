package handler

import (
	"fmt"
	"net/http"
	"testing"

	"video-support-agent/internal/llm"
)

func TestClassifyStreamErrors(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{
			name:       "stream unsupported",
			err:        llm.ErrStreamUnsupported,
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   "stream_unavailable",
		},
		{
			name:       "wrapped incomplete reply",
			err:        fmt.Errorf("stream failed: %w", llm.ErrIncompleteReply),
			wantStatus: http.StatusBadGateway,
			wantCode:   "model_incomplete_reply",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, response := classifyResponseError(tt.err)

			if status != tt.wantStatus {
				t.Fatalf("expected status %d, got %d", tt.wantStatus, status)
			}

			if response.Code != tt.wantCode {
				t.Fatalf("expected code %q, got %q", tt.wantCode, response.Code)
			}

			if response.Message == "" {
				t.Fatal("public error message must not be empty")
			}
		})
	}
}
