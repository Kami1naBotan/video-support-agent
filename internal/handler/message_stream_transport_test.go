package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"video-support-agent/internal/llm"
	"video-support-agent/internal/model"
	"video-support-agent/internal/service"
)

type flushFailureWriter struct {
	*httptest.ResponseRecorder
	flushCalls  int
	writeCalls  int
	failAt      int
	err         error
	writeFailAt int
}

func (w *flushFailureWriter) Write(data []byte) (int, error) {
	w.writeCalls++

	if w.writeFailAt > 0 && w.writeCalls == w.writeFailAt {
		return 0, w.err
	}

	return w.ResponseRecorder.Write(data)
}

func (w *flushFailureWriter) FlushError() error {
	w.flushCalls++

	if w.flushCalls == w.failAt {
		return w.err
	}

	w.ResponseRecorder.Flush()
	return nil
}

type transportStreamChat struct {
	testChatResponder
	calls        int
	callbackErr  error
	ctxAtFailure error
}

func (s *transportStreamChat) ReplyStream(
	ctx context.Context,
	_ []llm.HistoryMessage,
	_ string,
	onChunk func(string) error,
) (string, error) {
	s.calls++

	if err := onChunk("第一段"); err != nil {
		s.callbackErr = err

		// 必须在 Handler 返回前观察取消，
		// 防止将 Handler 的 defer cancel 误当成即时取消。
		s.ctxAtFailure = ctx.Err()

		return "", err
	}

	return "第一段", nil
}

func TestMessageHandlerTokenFlushFailure(t *testing.T) {
	flushErr := errors.New("test flush failure")

	messages := &handlerStreamSaveStore{}
	conversations := &testConversationStore{
		conversation: &model.Conversation{ID: 1, UserID: 7},
	}
	chat := &transportStreamChat{}

	svc := service.NewMessageService(
		messages,
		conversations,
		nil,
		nil,
		chat,
	)

	handler := NewMessageHandler(svc)
	router, token := newMessageTestRouter(t)
	router.POST(
		"/api/v1/conversations/:id/messages/stream",
		handler.CreateStream,
	)

	request := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/conversations/1/messages/stream",
		strings.NewReader(`{"content":"测试问题"}`),
	)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("Authorization", "Bearer "+token)

	writer := &flushFailureWriter{
		ResponseRecorder: httptest.NewRecorder(),
		failAt:           2,
		err:              flushErr,
	}

	router.ServeHTTP(writer, request)

	if chat.calls != 1 {
		t.Fatalf("expected one model call, got %d", chat.calls)
	}
	if !errors.Is(chat.callbackErr, flushErr) {
		t.Fatalf("expected flush error in callback, got %v", chat.callbackErr)
	}
	if !errors.Is(chat.ctxAtFailure, context.Canceled) {
		t.Fatalf(
			"model context must be canceled before callback returns: %v",
			chat.ctxAtFailure,
		)
	}

	if writer.flushCalls != 2 || writer.writeCalls != 2 {
		t.Fatalf(
			"must stop after token flush failure; writes=%d flushes=%d",
			writer.writeCalls,
			writer.flushCalls,
		)
	}

	if messages.saveCalls != 0 || messages.created != nil {
		t.Fatal("interrupted generation must not save")
	}

	body := writer.Body.String()
	for _, event := range []string{"error", "citation", "done"} {
		if strings.Contains(body, "event: "+event+"\n") {
			t.Fatalf("must not write %s after transport failure", event)
		}
	}
}

func TestMessageHandlerStreamWriteFailureBoundaries(t *testing.T) {
	tests := []struct {
		name           string
		failAtWrite    int
		wantModelCalls int
		wantSaveCalls  int
	}{
		{
			name:        "start write failure",
			failAtWrite: 1,
		},
		{
			name:           "token write failure",
			failAtWrite:    2,
			wantModelCalls: 1,
		},
		{
			name:           "done write failure after save",
			failAtWrite:    4,
			wantModelCalls: 1,
			wantSaveCalls:  1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			writeErr := errors.New("test transport write failure")

			messages := &handlerStreamSaveStore{}
			conversations := &testConversationStore{
				conversation: &model.Conversation{ID: 1, UserID: 7},
			}
			chat := &transportStreamChat{}

			svc := service.NewMessageService(
				messages,
				conversations,
				nil,
				nil,
				chat,
			)

			handler := NewMessageHandler(svc)
			router, token := newMessageTestRouter(t)
			router.POST(
				"/api/v1/conversations/:id/messages/stream",
				handler.CreateStream,
			)

			request := httptest.NewRequest(
				http.MethodPost,
				"/api/v1/conversations/1/messages/stream",
				strings.NewReader(`{"content":"测试问题"}`),
			)
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Accept", "text/event-stream")
			request.Header.Set("Authorization", "Bearer "+token)

			writer := &flushFailureWriter{
				ResponseRecorder: httptest.NewRecorder(),
				writeFailAt:      tt.failAtWrite,
				err:              writeErr,
			}

			router.ServeHTTP(writer, request)

			if chat.calls != tt.wantModelCalls {
				t.Fatalf(
					"expected %d model calls, got %d",
					tt.wantModelCalls,
					chat.calls,
				)
			}

			if messages.saveCalls != tt.wantSaveCalls {
				t.Fatalf(
					"expected %d save calls, got %d",
					tt.wantSaveCalls,
					messages.saveCalls,
				)
			}

			if writer.writeCalls != tt.failAtWrite {
				t.Fatalf(
					"must stop writing after failure: got %d calls",
					writer.writeCalls,
				)
			}
			if writer.flushCalls != tt.failAtWrite-1 {
				t.Fatalf(
					"failed write must not be flushed: got %d flushes",
					writer.flushCalls,
				)
			}

			if tt.failAtWrite == 2 {
				if !errors.Is(chat.callbackErr, writeErr) {
					t.Fatal("token write error must reach the model callback")
				}
				if !errors.Is(chat.ctxAtFailure, context.Canceled) {
					t.Fatal("token write failure must immediately cancel context")
				}
			}

			if tt.wantSaveCalls == 0 {
				if messages.created != nil {
					t.Fatal("interrupted generation must not create a message")
				}
			} else {
				if messages.created == nil ||
					messages.created.ID != 10 ||
					messages.created.Content != "测试问题" {
					t.Fatal("done delivery failure must preserve the saved result")
				}
				if chat.callbackErr != nil {
					t.Fatal("generation must have completed before done failed")
				}
			}

			body := writer.Body.String()
			if strings.Contains(body, "event: error\n") {
				t.Fatal("must not send an error event over the failed transport")
			}
			if strings.Contains(body, "event: done\n") {
				t.Fatal("done must not appear in the recorded successful writes")
			}

			if tt.failAtWrite == 1 && body != "" {
				t.Fatal("failed start write must not produce a response body")
			}
			if tt.failAtWrite == 4 &&
				!strings.Contains(body, "event: citation\n") {
				t.Fatal("expected citation before the failed done write")
			}
		})
	}
}
