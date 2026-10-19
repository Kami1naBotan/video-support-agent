package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"video-support-agent/internal/service"

	"github.com/gin-gonic/gin"
)

func TestWriteErrorConversationBusy(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{
			name: "direct error",
			err:  service.ErrConversationBusy,
		},
		{
			name: "wrapped error",
			err: fmt.Errorf(
				"internal processing detail: %w",
				service.ErrConversationBusy,
			),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)

			writeError(ctx, tt.err)

			if recorder.Code != http.StatusConflict {
				t.Fatalf(
					"expected status 409, got %d",
					recorder.Code,
				)
			}

			var response ErrorResponse
			if err := json.Unmarshal(
				recorder.Body.Bytes(),
				&response,
			); err != nil {
				t.Fatalf("decode response: %v", err)
			}

			if response.Code != "conversation_busy" {
				t.Fatalf("unexpected error code: %q", response.Code)
			}
			if response.Message != "该会话正在处理上一条消息，请等待完成后再发送" {
				t.Fatalf("unexpected error message: %q", response.Message)
			}
		})
	}
}
