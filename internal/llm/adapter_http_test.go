package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"video-support-agent/internal/config"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestModelAdapterHTTPAuthenticationFailure(t *testing.T) {
	var requests atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)

			if r.Method != http.MethodPost {
				t.Error("adapter must send a POST request")
			}
			if r.URL.Path != "/v1/chat/completions" {
				t.Error("adapter used an unexpected request path")
			}
			if r.Header.Get("Authorization") != "Bearer test-only-key" {
				t.Error("adapter did not send the configured test credential")
			}

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)

			_, _ = io.WriteString(w, `{
				"error": {
					"message": "private-provider-response-marker",
					"type": "authentication_error",
					"code": "invalid_api_key"
				}
			}`)
		},
	))
	defer server.Close()

	chatModel, err := NewChatModel(
		context.Background(),
		config.LLMConfig{
			BaseURL: server.URL + "/v1",
			Model:   "test-model",
			APIKey:  "test-only-key",
			Timeout: "2s",
		},
	)
	if err != nil {
		t.Fatalf("initialize adapter: %v", err)
	}

	core, logs := observer.New(zap.DebugLevel)

	client, err := NewChatClient(
		chatModel,
		2*time.Second,
		zap.New(core),
	)
	if err != nil {
		t.Fatalf("initialize chat client: %v", err)
	}

	answer, err := client.Reply(
		context.Background(),
		nil,
		"private-user-question-marker",
	)

	if !errors.Is(err, ErrModelFailed) {
		t.Fatal("HTTP 401 must become ErrModelFailed")
	}
	if err.Error() != ErrModelFailed.Error() {
		t.Fatal("public error must not include upstream details")
	}
	if answer != "" {
		t.Fatal("failed request must not return an answer")
	}

	if requests.Load() != 1 {
		t.Fatalf(
			"expected exactly 1 HTTP request, got %d",
			requests.Load(),
		)
	}

	entries := logs.All()
	if len(entries) != 1 {
		t.Fatalf("expected exactly 1 log entry, got %d", len(entries))
	}

	entry := entries[0]
	if entry.Level != zap.WarnLevel ||
		entry.Message != "model call failed" {
		t.Fatal("unexpected model failure log")
	}

	fields := entry.ContextMap()
	if len(fields) != 1 ||
		fields["error_kind"] != string(ErrorKindAuthentication) {
		t.Fatal("log must contain only the authentication category")
	}
}

func TestModelAdapterHTTPSuccess(t *testing.T) {
	var requests atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)

			if r.Method != http.MethodPost ||
				r.URL.Path != "/v1/chat/completions" {
				t.Error("unexpected model request method or path")
			}
			if r.Header.Get("Authorization") != "Bearer test-only-key" {
				t.Error("unexpected model request credential")
			}

			var body struct {
				Model     string `json:"model"`
				Stream    bool   `json:"stream"`
				MaxTokens *int   `json:"max_tokens"`
				Messages  []struct {
					Role    string `json:"role"`
					Content string `json:"content"`
				} `json:"messages"`
			}

			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error("adapter must send a valid JSON request")
				http.Error(w, "invalid request", http.StatusBadRequest)
				return
			}

			if body.Model != "test-model" {
				t.Error("adapter must use the configured model")
			}
			if body.Stream {
				t.Error("Generate must use a non-streaming request")
			}
			if body.MaxTokens == nil {
				t.Error("adapter must send max_tokens")
			} else if *body.MaxTokens != 1024 {
				t.Errorf("expected max_tokens 1024, got %d", *body.MaxTokens)
			}

			wantRoles := []string{
				"system",
				"user",
				"assistant",
				"user",
			}
			wantContents := []string{
				supportSystemPrompt,
				"我的投稿被退回了",
				"页面显示了什么提示？",
				"提示标题不符合要求",
			}

			if len(body.Messages) != len(wantRoles) {
				t.Errorf(
					"expected %d messages, got %d",
					len(wantRoles),
					len(body.Messages),
				)
				http.Error(w, "invalid messages", http.StatusBadRequest)
				return
			}

			for i, message := range body.Messages {
				if message.Role != wantRoles[i] {
					t.Errorf("unexpected role at message index %d", i)
				}
				if message.Content != wantContents[i] {
					t.Errorf("unexpected content at message index %d", i)
				}
			}

			w.Header().Set("Content-Type", "application/json")

			_, _ = io.WriteString(w, `{
				"id": "test-completion",
				"object": "chat.completion",
				"created": 1700000000,
				"model": "test-model",
				"choices": [
					{
						"index": 0,
						"message": {
							"role": "assistant",
							"content": "  请提供标题相关的完整提示。  "
						},
						"finish_reason": "stop"
					}
				],
				"usage": {
					"prompt_tokens": 20,
					"completion_tokens": 10,
					"total_tokens": 30
				}
			}`)
		},
	))
	defer server.Close()

	chatModel, err := NewChatModel(
		context.Background(),
		config.LLMConfig{
			BaseURL: server.URL + "/v1",
			Model:   "test-model",
			APIKey:  "test-only-key",
			Timeout: "2s",
		},
	)
	if err != nil {
		t.Fatalf("initialize adapter: %v", err)
	}

	client, err := NewChatClient(chatModel, 2*time.Second, nil)
	if err != nil {
		t.Fatalf("initialize chat client: %v", err)
	}

	answer, err := client.Reply(
		context.Background(),
		[]HistoryMessage{
			{
				Role:    "user",
				Content: "我的投稿被退回了",
			},
			{
				Role:    "assistant",
				Content: "页面显示了什么提示？",
			},
		},
		"  提示标题不符合要求  ",
	)
	if err != nil {
		t.Fatalf("model reply failed: %v", err)
	}

	if answer != "请提供标题相关的完整提示。" {
		t.Fatalf("unexpected answer: %q", answer)
	}
	if requests.Load() != 1 {
		t.Fatalf(
			"expected exactly 1 HTTP request, got %d",
			requests.Load(),
		)
	}
}

func TestModelAdapterHTTPStreamCancellation(t *testing.T) {
	firstChunk := make(chan string, 1)
	upstreamCanceled := make(chan error, 1)
	release := make(chan struct{})

	ctx, cancel := context.WithCancel(context.Background())

	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost ||
				r.URL.Path != "/v1/chat/completions" {
				t.Error("unexpected model request method or path")
				http.Error(w, "unexpected request", http.StatusBadRequest)
				return
			}

			var body struct {
				Stream bool `json:"stream"`
			}

			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error("invalid model request JSON")
				http.Error(w, "invalid request", http.StatusBadRequest)
				return
			}

			// 读完剩余请求体，便于服务端检测连接断开。
			if _, err := io.Copy(io.Discard, r.Body); err != nil {
				t.Error("cannot finish reading request body")
				return
			}

			if !body.Stream {
				t.Error("Stream must send stream=true")
				http.Error(w, "streaming required", http.StatusBadRequest)
				return
			}

			flusher, ok := w.(http.Flusher)
			if !ok {
				t.Error("response writer does not support flushing")
				http.Error(w, "streaming unavailable", http.StatusInternalServerError)
				return
			}

			w.Header().Set("Content-Type", "text/event-stream")

			_, err := io.WriteString(w,
				`data: {"id":"test-stream","object":"chat.completion.chunk","created":1700000000,"model":"test-model","choices":[{"index":0,"delta":{"role":"assistant","content":"第一段"},"finish_reason":null}]}`+
					"\n\n",
			)
			if err != nil {
				t.Error("cannot write first stream chunk")
				return
			}

			flusher.Flush()

			// 不发送结束标志，模拟模型正在等待生成下一段。
			select {
			case <-r.Context().Done():
				select {
				case upstreamCanceled <- r.Context().Err():
				default:
				}
			case <-release:
				// 测试失败时允许清理，避免 server.Close 卡住。
			}
		},
	))

	t.Cleanup(func() {
		cancel()
		close(release)
		server.Close()
	})

	chatModel, err := NewChatModel(
		context.Background(),
		config.LLMConfig{
			BaseURL: server.URL + "/v1",
			Model:   "test-model",
			APIKey:  "test-only-key",
			Timeout: "10s",
		},
	)
	if err != nil {
		t.Fatalf("initialize adapter: %v", err)
	}

	client, err := NewChatClient(chatModel, 10*time.Second, nil)
	if err != nil {
		t.Fatalf("initialize chat client: %v", err)
	}

	type replyResult struct {
		answer string
		err    error
	}
	done := make(chan replyResult, 1)

	go func() {
		answer, err := client.ReplyStream(
			ctx,
			nil,
			"如何查看投稿状态？",
			func(part string) error {
				select {
				case firstChunk <- part:
				default:
				}
				return nil
			},
		)

		done <- replyResult{answer: answer, err: err}
	}()

	select {
	case part := <-firstChunk:
		if part != "第一段" {
			t.Fatalf("unexpected first chunk: %q", part)
		}
	case result := <-done:
		t.Fatalf("stream ended before cancellation: %v", result.err)
	case <-time.After(3 * time.Second):
		t.Fatal("did not receive the first model chunk")
	}

	cancel()

	select {
	case result := <-done:
		if !errors.Is(result.err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", result.err)
		}
		if result.answer != "" {
			t.Fatal("canceled request must not return an answer")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("model call did not stop after cancellation")
	}

	select {
	case err := <-upstreamCanceled:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("unexpected upstream context error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("upstream did not observe HTTP cancellation")
	}
}

func TestModelAdapterHTTPCancellation(t *testing.T) {
	entered := make(chan struct{}, 1)
	upstreamCanceled := make(chan error, 1)
	release := make(chan struct{})

	ctx, cancel := context.WithCancel(context.Background())

	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost ||
				r.URL.Path != "/v1/chat/completions" {
				t.Error("unexpected model request method or path")
				http.Error(w, "unexpected request", http.StatusBadRequest)
				return
			}

			var body struct {
				Stream bool `json:"stream"`
			}

			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error("invalid model request JSON")
				http.Error(w, "invalid request", http.StatusBadRequest)
				return
			}

			if _, err := io.Copy(io.Discard, r.Body); err != nil {
				t.Error("cannot finish reading request body")
				return
			}

			if body.Stream {
				t.Error("Generate must use a non-streaming request")
				http.Error(w, "unexpected streaming request", http.StatusBadRequest)
				return
			}

			select {
			case entered <- struct{}{}:
			default:
			}

			// 模拟请求已到达，但模型尚未返回答案。
			select {
			case <-r.Context().Done():
				select {
				case upstreamCanceled <- r.Context().Err():
				default:
				}
			case <-release:
				// 测试失败时的清理出口。
			}
		},
	))

	t.Cleanup(func() {
		cancel()
		close(release)
		server.Close()
	})

	chatModel, err := NewChatModel(
		context.Background(),
		config.LLMConfig{
			BaseURL: server.URL + "/v1",
			Model:   "test-model",
			APIKey:  "test-only-key",
			Timeout: "10s",
		},
	)
	if err != nil {
		t.Fatalf("initialize adapter: %v", err)
	}

	client, err := NewChatClient(chatModel, 10*time.Second, nil)
	if err != nil {
		t.Fatalf("initialize chat client: %v", err)
	}

	type replyResult struct {
		answer string
		err    error
	}
	done := make(chan replyResult, 1)

	go func() {
		answer, err := client.Reply(
			ctx,
			nil,
			"如何查看投稿状态？",
		)

		done <- replyResult{
			answer: answer,
			err:    err,
		}
	}()

	select {
	case <-entered:
		// 请求已经到达上游，可以测试进行中的 HTTP 取消。
	case result := <-done:
		t.Fatalf("model call ended before cancellation: %v", result.err)
	case <-time.After(3 * time.Second):
		t.Fatal("model request did not reach upstream")
	}

	cancel()

	select {
	case result := <-done:
		if !errors.Is(result.err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", result.err)
		}
		if result.answer != "" {
			t.Fatal("canceled request must not return an answer")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("model call did not stop after cancellation")
	}

	select {
	case err := <-upstreamCanceled:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("unexpected upstream context error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("upstream did not observe HTTP cancellation")
	}
}

func TestModelAdapterHTTPStreamSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost ||
				r.URL.Path != "/v1/chat/completions" {
				t.Error("unexpected model request method or path")
				http.Error(w, "unexpected request", http.StatusBadRequest)
				return
			}

			var body struct {
				Stream bool `json:"stream"`
			}

			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error("invalid model request JSON")
				http.Error(w, "invalid request", http.StatusBadRequest)
				return
			}

			if !body.Stream {
				t.Error("Stream must send stream=true")
				http.Error(w, "streaming required", http.StatusBadRequest)
				return
			}

			flusher, ok := w.(http.Flusher)
			if !ok {
				t.Error("response writer does not support flushing")
				return
			}

			w.Header().Set("Content-Type", "text/event-stream")

			frames := []string{
				`{"id":"test-stream","object":"chat.completion.chunk","created":1700000000,"model":"test-model","choices":[{"index":0,"delta":{"role":"assistant","content":"你好"},"finish_reason":null}]}`,
				`{"id":"test-stream","object":"chat.completion.chunk","created":1700000000,"model":"test-model","choices":[{"index":0,"delta":{"content":" world\n下一段"},"finish_reason":null}]}`,
				`{"id":"test-stream","object":"chat.completion.chunk","created":1700000000,"model":"test-model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
				`[DONE]`,
			}

			for _, frame := range frames {
				if _, err := io.WriteString(
					w,
					"data: "+frame+"\n\n",
				); err != nil {
					t.Error("cannot write stream frame")
					return
				}

				flusher.Flush()
			}
		},
	))
	defer server.Close()

	chatModel, err := NewChatModel(
		context.Background(),
		config.LLMConfig{
			BaseURL: server.URL + "/v1",
			Model:   "test-model",
			APIKey:  "test-only-key",
			Timeout: "3s",
		},
	)
	if err != nil {
		t.Fatalf("initialize adapter: %v", err)
	}

	client, err := NewChatClient(chatModel, 3*time.Second, nil)
	if err != nil {
		t.Fatalf("initialize chat client: %v", err)
	}

	var received []string

	answer, err := client.ReplyStream(
		context.Background(),
		nil,
		"你好",
		func(part string) error {
			received = append(received, part)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("stream reply failed: %v", err)
	}

	wantParts := []string{"你好", " world\n下一段"}
	if !reflect.DeepEqual(received, wantParts) {
		t.Fatalf("expected chunks %#v, got %#v", wantParts, received)
	}

	wantAnswer := "你好 world\n下一段"
	if answer != wantAnswer {
		t.Fatalf("expected answer %q, got %q", wantAnswer, answer)
	}
}
