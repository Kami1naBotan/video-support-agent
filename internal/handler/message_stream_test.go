package handler

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
	"video-support-agent/internal/llm"
	"video-support-agent/internal/model"
	"video-support-agent/internal/service"
)

type handlerStreamChatStub struct {
	testChatResponder
}

func (handlerStreamChatStub) ReplyStream(
	ctx context.Context,
	history []llm.HistoryMessage,
	question string,
	onChunk func(string) error,
) (string, error) {
	parts := []string{"你好", "\n请描述你的问题"}

	for _, part := range parts {
		if err := onChunk(part); err != nil {
			return "", err
		}
	}

	return strings.Join(parts, ""), nil
}

var _ service.ChatStreamResponder = handlerStreamChatStub{}

func TestMessageHandlerCreateStreamSuccess(t *testing.T) {
	messages := &testMessageStore{}
	conversations := &testConversationStore{
		conversation: &model.Conversation{ID: 1, UserID: 7},
	}

	svc := service.NewMessageService(
		messages,
		conversations,
		nil,
		nil,
		handlerStreamChatStub{},
	)
	h := NewMessageHandler(svc)

	router, token := newMessageTestRouter(t)
	router.POST(
		"/api/v1/conversations/:id/messages/stream",
		h.CreateStream,
	)

	request := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/conversations/1/messages/stream",
		strings.NewReader(`{"content":"你好"}`),
	)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "text/event-stream")

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf(
			"expected status 200, got %d: %s",
			recorder.Code,
			recorder.Body.String(),
		)
	}

	if got := recorder.Header().Get("Content-Type"); got !=
		"text/event-stream; charset=utf-8" {
		t.Fatalf("unexpected content type: %q", got)
	}

	if got := recorder.Header().Get("Cache-Control"); got !=
		"no-cache, no-transform" {
		t.Fatalf("unexpected cache control: %q", got)
	}

	if got := recorder.Header().Get("X-Accel-Buffering"); got != "no" {
		t.Fatalf("unexpected buffering header: %q", got)
	}

	if !recorder.Flushed {
		t.Fatal("SSE response must be flushed")
	}

	body := recorder.Body.String()
	if !strings.HasSuffix(body, "\n\n") {
		t.Fatal("last SSE event must end with a blank line")
	}

	frames := strings.Split(
		strings.TrimSuffix(body, "\n\n"),
		"\n\n",
	)

	wantEvents := []string{
		"start",
		"token",
		"token",
		"citation",
		"done",
	}
	if len(frames) != len(wantEvents) {
		t.Fatalf("expected 5 events, got %d: %q", len(frames), body)
	}

	payloads := make([]json.RawMessage, 0, len(frames))

	for i, frame := range frames {
		lines := strings.Split(frame, "\n")
		if len(lines) != 2 {
			t.Fatalf("unexpected frame format: %q", frame)
		}

		if lines[0] != "event: "+wantEvents[i] {
			t.Fatalf("unexpected event at index %d: %q", i, lines[0])
		}

		if !strings.HasPrefix(lines[1], "data: ") {
			t.Fatalf("missing data field: %q", frame)
		}

		data := strings.TrimPrefix(lines[1], "data: ")
		if !json.Valid([]byte(data)) {
			t.Fatalf("invalid event JSON: %q", data)
		}
		payloads = append(payloads, json.RawMessage(data))
	}

	var start struct {
		ConversationID uint64 `json:"conversation_id"`
	}
	if err := json.Unmarshal(payloads[0], &start); err != nil {
		t.Fatal(err)
	}
	if start.ConversationID != 1 {
		t.Fatalf("unexpected conversation ID: %d", start.ConversationID)
	}

	var parts []string
	for _, payload := range payloads[1:3] {
		var tokenData struct {
			Content string `json:"content"`
		}
		if err := json.Unmarshal(payload, &tokenData); err != nil {
			t.Fatal(err)
		}
		parts = append(parts, tokenData.Content)
	}

	if !reflect.DeepEqual(parts, []string{"你好", "\n请描述你的问题"}) {
		t.Fatalf("unexpected streamed content: %#v", parts)
	}

	var citation struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(payloads[3], &citation); err != nil {
		t.Fatal(err)
	}
	if citation.Items == nil || len(citation.Items) != 0 {
		t.Fatal("citation items must be an empty array")
	}

	var done struct {
		UserMessageID      uint64 `json:"user_message_id"`
		AssistantMessageID uint64 `json:"assistant_message_id"`
	}
	if err := json.Unmarshal(payloads[4], &done); err != nil {
		t.Fatal(err)
	}

	if messages.created == nil {
		t.Fatal("successful response must save the message pair")
	}
	if done.UserMessageID != 10 || done.AssistantMessageID != 11 {
		t.Fatalf("unexpected saved message IDs: %+v", done)
	}
}

type handlerFailingStreamChat struct {
	testChatResponder
	err error
}

func (s handlerFailingStreamChat) ReplyStream(
	ctx context.Context,
	history []llm.HistoryMessage,
	question string,
	onChunk func(string) error,
) (string, error) {
	if err := onChunk("已经生成的内容"); err != nil {
		return "", err
	}

	if s.err != nil {
		return "", s.err
	}

	return "已经生成的内容", nil
}

type handlerStreamSaveStore struct {
	testMessageStore
	saveErr   error
	saveCalls int
}

func (s *handlerStreamSaveStore) CreatePair(
	ctx context.Context,
	userMessage *model.Message,
	assistantMessage *model.Message,
) error {
	s.saveCalls++

	if s.saveErr != nil {
		return s.saveErr
	}

	return s.testMessageStore.CreatePair(
		ctx,
		userMessage,
		assistantMessage,
	)
}

func TestMessageHandlerCreateStreamErrors(t *testing.T) {
	tests := []struct {
		name          string
		ownerID       uint64
		modelErr      error
		saveErr       error
		wantStatus    int
		wantCode      string
		wantStream    bool
		wantSaveCalls int
	}{
		{
			name:       "reject before stream",
			ownerID:    8,
			wantStatus: http.StatusNotFound,
			wantCode:   "conversation_not_found",
		},
		{
			name:       "model timeout after token",
			ownerID:    7,
			modelErr:   llm.ErrModelTimeout,
			wantStatus: http.StatusOK,
			wantCode:   "model_timeout",
			wantStream: true,
		},
		{
			name:          "save fails after generation",
			ownerID:       7,
			saveErr:       errors.New("private-db-error-marker"),
			wantStatus:    http.StatusOK,
			wantCode:      "internal_error",
			wantStream:    true,
			wantSaveCalls: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			messages := &handlerStreamSaveStore{
				saveErr: tt.saveErr,
			}
			conversations := &testConversationStore{
				conversation: &model.Conversation{
					ID:     1,
					UserID: tt.ownerID,
				},
			}

			svc := service.NewMessageService(
				messages,
				conversations,
				nil,
				nil,
				handlerFailingStreamChat{err: tt.modelErr},
			)
			h := NewMessageHandler(svc)

			router, token := newMessageTestRouter(t)
			router.POST(
				"/api/v1/conversations/:id/messages/stream",
				h.CreateStream,
			)

			request := httptest.NewRequest(
				http.MethodPost,
				"/api/v1/conversations/1/messages/stream",
				strings.NewReader(`{"content":"你好"}`),
			)
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", "Bearer "+token)

			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)

			body := recorder.Body.String()

			if recorder.Code != tt.wantStatus {
				t.Fatalf(
					"expected status %d, got %d: %s",
					tt.wantStatus, recorder.Code, body,
				)
			}

			if messages.saveCalls != tt.wantSaveCalls {
				t.Fatalf(
					"expected %d save calls, got %d",
					tt.wantSaveCalls, messages.saveCalls,
				)
			}
			if messages.created != nil {
				t.Fatal("failed request must not have a saved message")
			}

			if strings.Contains(body, "event: done\n") {
				t.Fatal("failed request must not send done")
			}
			if strings.Contains(body, "private-db-error-marker") {
				t.Fatal("response must not expose database details")
			}

			var response ErrorResponse

			if !tt.wantStream {
				if !strings.HasPrefix(
					recorder.Header().Get("Content-Type"),
					"application/json",
				) {
					t.Fatal("pre-stream error must be JSON")
				}
				if recorder.Flushed || strings.Contains(body, "event:") {
					t.Fatal("rejected request must not start SSE")
				}

				if err := json.Unmarshal([]byte(body), &response); err != nil {
					t.Fatalf("decode JSON error: %v", err)
				}
			} else {
				if !strings.HasPrefix(
					recorder.Header().Get("Content-Type"),
					"text/event-stream",
				) {
					t.Fatal("post-start error must remain SSE")
				}
				if !recorder.Flushed {
					t.Fatal("SSE error must be flushed")
				}
				if !strings.HasSuffix(body, "\n\n") {
					t.Fatal("SSE error must end with a blank line")
				}

				frames := strings.Split(
					strings.TrimSuffix(body, "\n\n"),
					"\n\n",
				)
				wantEvents := []string{"start", "token", "error"}

				if len(frames) != len(wantEvents) {
					t.Fatalf("unexpected event sequence: %q", body)
				}

				for i, name := range wantEvents {
					if !strings.HasPrefix(frames[i], "event: "+name+"\n") {
						t.Fatalf("unexpected event at %d: %q", i, frames[i])
					}
				}

				payload := strings.TrimPrefix(
					frames[2],
					"event: error\ndata: ",
				)
				if err := json.Unmarshal([]byte(payload), &response); err != nil {
					t.Fatalf("decode SSE error: %v", err)
				}
			}

			if response.Code != tt.wantCode {
				t.Fatalf(
					"expected error code %q, got %q",
					tt.wantCode, response.Code,
				)
			}
		})
	}
}

type disconnectStreamChat struct {
	testChatResponder

	canceled chan error
	release  <-chan struct{}
}

func (s *disconnectStreamChat) ReplyStream(
	ctx context.Context,
	history []llm.HistoryMessage,
	question string,
	onChunk func(string) error,
) (string, error) {
	if err := onChunk("第一段"); err != nil {
		return "", err
	}

	select {
	case <-ctx.Done():
		s.canceled <- ctx.Err()
		return "", ctx.Err()
	case <-s.release:
		// 仅在测试清理时使用，不能当作取消验证通过。
		return "", errors.New("test cleanup")
	}
}

func TestMessageHandlerStreamClientDisconnect(t *testing.T) {
	messages := &handlerStreamSaveStore{}
	conversations := &testConversationStore{
		conversation: &model.Conversation{ID: 1, UserID: 7},
	}

	release := make(chan struct{})
	modelCanceled := make(chan error, 1)
	handlerDone := make(chan struct{})

	chat := &disconnectStreamChat{
		canceled: modelCanceled,
		release:  release,
	}

	svc := service.NewMessageService(
		messages, conversations, nil, nil, chat,
	)
	h := NewMessageHandler(svc)

	router, token := newMessageTestRouter(t)
	router.POST(
		"/api/v1/conversations/:id/messages/stream",
		h.CreateStream,
	)

	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			defer close(handlerDone)
			router.ServeHTTP(w, r)
		},
	))

	requestCtx, cancelRequest := context.WithTimeout(
		context.Background(),
		15*time.Second,
	)

	t.Cleanup(func() {
		cancelRequest()
		close(release)
		server.Close()
	})

	request, err := http.NewRequestWithContext(
		requestCtx,
		http.MethodPost,
		server.URL+"/api/v1/conversations/1/messages/stream",
		strings.NewReader(`{"content":"你好"}`),
	)
	if err != nil {
		t.Fatal(err)
	}

	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("Authorization", "Bearer "+token)

	client := server.Client()
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("send request: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", response.StatusCode)
	}

	// 读取到完整的第一条 token 事件，确认流已经开始。
	scanner := bufio.NewScanner(response.Body)
	currentEvent := ""
	sawTokenData := false
	receivedToken := false

	for scanner.Scan() {
		line := scanner.Text()

		if strings.HasPrefix(line, "event: ") {
			currentEvent = strings.TrimPrefix(line, "event: ")
		}

		if currentEvent == "token" &&
			strings.HasPrefix(line, "data: ") {
			var payload struct {
				Content string `json:"content"`
			}

			if err := json.Unmarshal(
				[]byte(strings.TrimPrefix(line, "data: ")),
				&payload,
			); err != nil {
				t.Fatalf("decode token: %v", err)
			}

			if payload.Content != "第一段" {
				t.Fatalf("unexpected token: %q", payload.Content)
			}
			sawTokenData = true
		}

		if line == "" {
			if currentEvent == "token" && sawTokenData {
				receivedToken = true
				break
			}
			currentEvent = ""
		}
	}

	if err := scanner.Err(); err != nil {
		t.Fatalf("read stream: %v", err)
	}
	if !receivedToken {
		t.Fatal("did not receive the first complete token event")
	}

	// 主动关闭尚未结束的响应，模拟客户端离开。
	if err := response.Body.Close(); err != nil {
		t.Fatalf("close response body: %v", err)
	}

	select {
	case err := <-modelCanceled:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected model cancellation, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("model did not observe client disconnect")
	}

	select {
	case <-handlerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not exit after client disconnect")
	}

	// Handler 退出后再读取替身状态，避免并发读写。
	if messages.saveCalls != 0 {
		t.Fatalf(
			"interrupted generation must not save, got %d save calls",
			messages.saveCalls,
		)
	}
	if messages.created != nil {
		t.Fatal("interrupted generation must not create a message")
	}
}
