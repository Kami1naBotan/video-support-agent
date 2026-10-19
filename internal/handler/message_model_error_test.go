package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"video-support-agent/internal/llm"
	"video-support-agent/internal/model"
	"video-support-agent/internal/service"
)

type failingChatResponder struct {
	err   error
	calls int
}

func (f *failingChatResponder) Reply(
	_ context.Context,
	_ []llm.HistoryMessage,
	_ string,
) (string, error) {
	f.calls++
	return "", f.err
}

func TestMessageHandlerModelErrors(t *testing.T) {
	tests := []struct {
		name        string
		modelErr    error
		wantStatus  int
		wantCode    string
		wantMessage string
	}{
		{
			name:        "model timeout",
			modelErr:    llm.ErrModelTimeout,
			wantStatus:  http.StatusGatewayTimeout,
			wantCode:    "model_timeout",
			wantMessage: "模型响应超时，请稍后重试",
		},
		{
			name:        "model unavailable",
			modelErr:    llm.ErrModelFailed,
			wantStatus:  http.StatusBadGateway,
			wantCode:    "model_unavailable",
			wantMessage: "模型服务暂时不可用，请稍后重试",
		},
		{
			name:        "empty model reply",
			modelErr:    llm.ErrEmptyReply,
			wantStatus:  http.StatusBadGateway,
			wantCode:    "model_empty_reply",
			wantMessage: "模型未返回有效回答，请稍后重试",
		},
		{
			name: "wrapped model error",
			modelErr: fmt.Errorf(
				"internal-provider-detail: %w",
				llm.ErrModelFailed,
			),
			wantStatus:  http.StatusBadGateway,
			wantCode:    "model_unavailable",
			wantMessage: "模型服务暂时不可用，请稍后重试",
		},
		{
			name:        "model input too large",
			modelErr:    llm.ErrInputTooLarge,
			wantStatus:  http.StatusBadRequest,
			wantCode:    "model_input_too_large",
			wantMessage: "问题内容超过模型输入预算，请缩短后重试",
		},
		{
			name: "wrapped input budget error",
			modelErr: fmt.Errorf(
				"internal-provider-detail: %w",
				llm.ErrInputTooLarge,
			),
			wantStatus:  http.StatusBadRequest,
			wantCode:    "model_input_too_large",
			wantMessage: "问题内容超过模型输入预算，请缩短后重试",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			messages := &testMessageStore{}
			conversations := &testConversationStore{
				conversation: &model.Conversation{
					ID:     1,
					UserID: 7,
				},
			}
			chat := &failingChatResponder{err: tt.modelErr}

			svc := service.NewMessageService(
				messages,
				conversations,
				nil,
				nil,
				chat,
			)
			h := NewMessageHandler(svc)

			router, token := newMessageTestRouter(t)
			router.POST(
				"/api/v1/conversations/:id/messages",
				h.Create,
			)

			request := httptest.NewRequest(
				http.MethodPost,
				"/api/v1/conversations/1/messages",
				strings.NewReader(`{"content":"我的投稿被退回了"}`),
			)
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", "Bearer "+token)

			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)

			if recorder.Code != tt.wantStatus {
				t.Fatalf(
					"expected status %d, got %d; body=%s",
					tt.wantStatus,
					recorder.Code,
					recorder.Body.String(),
				)
			}

			var response ErrorResponse
			if err := json.Unmarshal(
				recorder.Body.Bytes(),
				&response,
			); err != nil {
				t.Fatalf("decode error response: %v", err)
			}

			if response.Code != tt.wantCode {
				t.Fatalf(
					"expected code %q, got %q",
					tt.wantCode,
					response.Code,
				)
			}
			if response.Message != tt.wantMessage {
				t.Fatalf(
					"expected message %q, got %q",
					tt.wantMessage,
					response.Message,
				)
			}
			if strings.Contains(
				recorder.Body.String(),
				"internal-provider-detail",
			) {
				t.Fatal("response must not expose internal error details")
			}

			if chat.calls != 1 {
				t.Fatalf("expected 1 model call, got %d", chat.calls)
			}
			if messages.created != nil {
				t.Fatal("model failure must not save a message")
			}
		})
	}
}
