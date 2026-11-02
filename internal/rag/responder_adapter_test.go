package rag_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"video-support-agent/internal/config"
	"video-support-agent/internal/llm"
	"video-support-agent/internal/rag"
	"video-support-agent/internal/retrieval"
)

func TestRAGResponderHTTPAdapter(t *testing.T) {
	cases := []struct {
		name   string
		status int
		finish string
		want   error
	}{
		{"complete", http.StatusOK, "stop", nil},
		{"truncated", http.StatusOK, "length", rag.ErrInvalidAnswer},
		{"authentication_failure", http.StatusUnauthorized, "", rag.ErrGenerationFailed},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			item := fixtureItem(1)
			rawOutput := outputJSON(t, "answer", normalAnswerRow())
			var requests atomic.Int32

			server := httptest.NewServer(http.HandlerFunc(func(
				w http.ResponseWriter,
				request *http.Request,
			) {
				requests.Add(1)
				if request.Method != http.MethodPost ||
					request.URL.Path != "/v1/chat/completions" {
					t.Error("unexpected adapter method or path")
					http.Error(w, "unexpected route", http.StatusBadRequest)
					return
				}
				if request.Header.Get("Authorization") != "Bearer test-only-key" {
					t.Error("configured test credential missing")
					http.Error(w, "unexpected credential", http.StatusBadRequest)
					return
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
				if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
					t.Errorf("decode request: %v", err)
					http.Error(w, "invalid request", http.StatusBadRequest)
					return
				}
				if body.Model != "test-model" ||
					body.Stream ||
					body.MaxTokens == nil ||
					*body.MaxTokens != 1024 {
					t.Error("unexpected model, streaming mode, or output limit")
					http.Error(w, "unexpected settings", http.StatusBadRequest)
					return
				}
				if len(body.Messages) != 2 ||
					body.Messages[0].Role != "system" ||
					body.Messages[0].Content == "" ||
					body.Messages[1].Role != "user" {
					t.Error("unexpected model messages")
					http.Error(w, "unexpected messages", http.StatusBadRequest)
					return
				}

				var current struct {
					Question string `json:"question"`
					Rules    []struct {
						RefID          string `json:"ref_id"`
						DocumentID     string `json:"document_id"`
						VersionID      string `json:"version_id"`
						ArticleID      string `json:"article_id"`
						ArticleContent string `json:"article_content"`
					} `json:"rules"`
				}
				if err := json.Unmarshal(
					[]byte(body.Messages[1].Content), &current,
				); err != nil {
					t.Errorf("decode current message: %v", err)
					http.Error(w, "invalid current message", http.StatusBadRequest)
					return
				}
				if current.Question != "本地协议测试问题" ||
					len(current.Rules) != 1 ||
					current.Rules[0].RefID != "R1" ||
					current.Rules[0].DocumentID != "201" ||
					current.Rules[0].VersionID != "301" ||
					current.Rules[0].ArticleID != "401" ||
					current.Rules[0].ArticleContent != item.Rule.ArticleContent {
					t.Error("current question or full rule changed in adapter")
					http.Error(w, "unexpected current input", http.StatusBadRequest)
					return
				}

				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				if tc.status != http.StatusOK {
					if err := json.NewEncoder(w).Encode(map[string]any{
						"error": map[string]any{
							"message": "PRIVATE_PROVIDER_MARKER",
							"type":    "authentication_error",
							"code":    "invalid_api_key",
						},
					}); err != nil {
						t.Errorf("write error response: %v", err)
					}
					return
				}

				if err := json.NewEncoder(w).Encode(map[string]any{
					"id":     "local-rag-test",
					"object": "chat.completion",
					"model":  "test-model",
					"choices": []any{
						map[string]any{
							"index": 0,
							"message": map[string]any{
								"role":    "assistant",
								"content": rawOutput,
							},
							"finish_reason": tc.finish,
						},
					},
				}); err != nil {
					t.Errorf("write completion response: %v", err)
				}
			}))
			defer server.Close()

			chatModel, err := llm.NewChatModel(
				context.Background(),
				config.LLMConfig{
					BaseURL: server.URL + "/v1",
					Model:   "test-model",
					APIKey:  "test-only-key",
					Timeout: "2s",
				},
			)
			if err != nil {
				t.Fatal(err)
			}

			retrieveCalls := 0
			retriever := responderRetrieverFunc(func(
				_ context.Context,
				query string,
				options retrieval.HybridOptions,
			) (retrieval.HybridResult, error) {
				retrieveCalls++
				if query != "本地协议测试问题" ||
					options != (retrieval.HybridOptions{TopK: 5}) {
					t.Errorf("query=%q options=%+v", query, options)
				}
				return fixtureResult(item), nil
			})
			responder, err := rag.NewResponder(
				retriever, chatModel, 2*time.Second, nil,
			)
			if err != nil {
				t.Fatal(err)
			}

			parent, cancel := context.WithTimeout(
				context.Background(), 10*time.Second,
			)
			defer cancel()
			reply, err := responder.ReplyGrounded(
				parent, nil, "  本地协议测试问题  ",
			)

			if tc.want != nil {
				requireAnswerError(t, reply, err, tc.want)
				if strings.Contains(err.Error(), "PRIVATE_PROVIDER_MARKER") {
					t.Fatal("provider details escaped")
				}
				if tc.status == http.StatusUnauthorized &&
					!errors.Is(err, rag.ErrGenerationFailed) {
					t.Fatalf("unexpected HTTP failure mapping: %v", err)
				}
			} else {
				requireResponderAnswer(t, reply, err)
				if reply.Citations[0].SourceURL != item.Rule.Chunk.SourceURL {
					t.Fatal("server-owned source snapshot changed")
				}
			}

			if retrieveCalls != 1 || requests.Load() != 1 {
				t.Fatalf(
					"retrieve=%d HTTP=%d; want exactly one each",
					retrieveCalls, requests.Load(),
				)
			}
			if parent.Err() != nil {
				t.Fatalf("parent unexpectedly ended: %v", parent.Err())
			}
		})
	}
}
