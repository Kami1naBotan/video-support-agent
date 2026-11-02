package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"video-support-agent/internal/model"
	"video-support-agent/internal/rag"
	"video-support-agent/internal/retrieval"
	"video-support-agent/internal/service"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/gin-gonic/gin"
)

const ragHTTPMessagePath = "/api/v1/conversations/1/messages"

type ragHTTPRetriever func(
	context.Context, string, retrieval.HybridOptions,
) (retrieval.HybridResult, error)

func (f ragHTTPRetriever) Retrieve(
	ctx context.Context, query string, options retrieval.HybridOptions,
) (retrieval.HybridResult, error) {
	return f(ctx, query, options)
}

type ragHTTPGenerator func(
	context.Context, []*schema.Message,
) (*schema.Message, error)

func (f ragHTTPGenerator) Generate(
	ctx context.Context, messages []*schema.Message, _ ...einomodel.Option,
) (*schema.Message, error) {
	return f(ctx, messages)
}

type ragHTTPStore struct {
	testMessageStore
	attempts int
	saveErr  error
	reply    *model.Message
	before   func(context.Context) error
}

func (s *ragHTTPStore) CreatePair(
	ctx context.Context, user, assistant *model.Message,
) error {
	s.attempts++
	if s.before != nil {
		if err := s.before(ctx); err != nil {
			return err
		}
	}
	if s.saveErr != nil {
		return s.saveErr
	}
	if err := s.testMessageStore.CreatePair(ctx, user, assistant); err != nil {
		return err
	}
	s.reply = assistant
	s.messages = append(s.messages, *user, *assistant)
	return nil
}

type ragHTTPHarness struct {
	store         *ragHTTPStore
	retrieved     retrieval.HybridResult
	retrieveErr   error
	raw           *schema.Message
	generateErr   error
	retrieveCalls int
	generateCalls int
}

func newRAGHTTPHarness() *ragHTTPHarness {
	at := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	return &ragHTTPHarness{
		store: &ragHTTPStore{
			testMessageStore: testMessageStore{
				messages: []model.Message{
					{ID: 1, ConversationID: 1, Role: "user", Content: "旧问题"},
					{ID: 2, ConversationID: 1, Role: "assistant", Content: "旧回答"},
				},
			},
		},
		retrieved: retrieval.HybridResult{
			Items: []retrieval.HybridItem{
				{
					Rule: retrieval.HydratedChunk{
						Chunk: retrieval.RuleChunk{
							ChunkID:       501,
							DocumentID:    9007199254740993,
							VersionID:     9007199254740995,
							ArticleID:     9007199254740997,
							CategoryID:    1,
							ArticleOrder:  1,
							Version:       "test-v1",
							Title:         "虚构规则",
							ArticleNumber: "TEST-1",
							Content:       "虚构检索片段",
							SourceURL:     "https://www.bilibili.com/__rag_fixture__",
							EffectiveAt:   at.Add(-time.Hour),
						},
						ArticleContent: "虚构测试条款：满足条件A时可提交材料。",
					},
					ChunkIDs: []uint64{501},
				},
			},
			RetrievedAt: at,
			Outcome:     retrieval.HybridSuccess,
			Stats:       retrieval.HybridStats{FinalCount: 1},
		},
		raw: &schema.Message{
			Role: schema.Assistant,
			Content: `{"decision":"answer","statements":[` +
				`{"text":"虚构测试陈述","evidence":[` +
				`{"ref_id":"R1","quote":"满足条件A时可提交材料"}]}]}`,
			ResponseMeta: &schema.ResponseMeta{FinishReason: "stop"},
		},
	}
}

func ragHTTPExpectedCitations() model.MessageCitations {
	at := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	return model.MessageCitations{
		{
			RefID:         "R1",
			DocumentID:    "9007199254740993",
			VersionID:     "9007199254740995",
			ArticleID:     "9007199254740997",
			CategoryID:    "1",
			ChunkID:       "501",
			ChunkIDs:      []string{"501"},
			Version:       "test-v1",
			Title:         "虚构规则",
			ArticleNumber: "TEST-1",
			SourceURL:     "https://www.bilibili.com/__rag_fixture__",
			EffectiveAt:   at.Add(-time.Hour),
			RetrievedAt:   at,
		},
	}
}

func (h *ragHTTPHarness) router(t *testing.T) (*gin.Engine, string) {
	t.Helper()
	responder, err := rag.NewResponder(
		ragHTTPRetriever(func(
			_ context.Context, query string, options retrieval.HybridOptions,
		) (retrieval.HybridResult, error) {
			h.retrieveCalls++
			if query != "测试问题" || options != (retrieval.HybridOptions{TopK: 5}) {
				t.Errorf("query=%q options=%+v", query, options)
			}
			return h.retrieved, h.retrieveErr
		}),
		ragHTTPGenerator(func(
			_ context.Context, messages []*schema.Message,
		) (*schema.Message, error) {
			h.generateCalls++
			if len(messages) != 4 ||
				messages[0].Role != schema.System ||
				messages[1].Content != "旧问题" ||
				messages[2].Content != "旧回答" ||
				messages[3].Role != schema.User {
				t.Error("history/current message integration changed")
			}
			return h.raw, h.generateErr
		}),
		2*time.Second,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	svc := service.NewMessageService(
		h.store,
		&testConversationStore{
			conversation: &model.Conversation{ID: 1, UserID: 7},
		},
		nil, nil, responder,
	)
	handler := NewMessageHandler(svc)
	router, token := newMessageTestRouter(t)
	router.POST("/api/v1/conversations/:id/messages", handler.Create)
	router.POST("/api/v1/conversations/:id/messages/stream", handler.CreateStream)
	router.GET("/api/v1/conversations/:id/messages", handler.List)
	return router, token
}

func ragHTTPRequest(
	ctx context.Context, token, method, path string,
) *http.Request {
	body := ""
	if method == http.MethodPost {
		body = `{"content":"  测试问题  "}`
	}
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request = request.WithContext(ctx)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	return request
}

func ragHTTPFrames(
	t *testing.T,
	recorder *httptest.ResponseRecorder,
	want ...string,
) []json.RawMessage {
	t.Helper()
	body := recorder.Body.String()
	if !strings.HasSuffix(body, "\n\n") {
		t.Fatalf("incomplete SSE frame: %q", body)
	}
	frames := strings.Split(strings.TrimSuffix(body, "\n\n"), "\n\n")
	if len(frames) != len(want) {
		t.Fatalf("frames=%d want=%d body=%s", len(frames), len(want), body)
	}
	result := make([]json.RawMessage, len(frames))
	for i, frame := range frames {
		lines := strings.Split(frame, "\n")
		if len(lines) != 2 ||
			lines[0] != "event: "+want[i] ||
			!strings.HasPrefix(lines[1], "data: ") {
			t.Fatalf("unexpected frame %d: %q", i, frame)
		}
		result[i] = json.RawMessage(strings.TrimPrefix(lines[1], "data: "))
		if !json.Valid(result[i]) {
			t.Fatalf("invalid event JSON: %s", result[i])
		}
	}
	return result
}

func ragHTTPHistory(
	t *testing.T,
	router http.Handler,
	token string,
	wantContent string,
	wantCitations model.MessageCitations,
) {
	t.Helper()
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, ragHTTPRequest(
		context.Background(), token, http.MethodGet, ragHTTPMessagePath,
	))
	if recorder.Code != http.StatusOK {
		t.Fatalf("history status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var messages []model.Message
	if err := json.Unmarshal(recorder.Body.Bytes(), &messages); err != nil {
		t.Fatal(err)
	}
	if len(messages) != 4 ||
		messages[2].ID != 10 ||
		messages[2].Role != "user" ||
		messages[2].Content != "测试问题" ||
		messages[3].ID != 11 ||
		messages[3].Role != "assistant" ||
		messages[3].Content != wantContent ||
		!reflect.DeepEqual(messages[3].Citations, wantCitations) {
		t.Fatalf("unexpected history: %+v", messages)
	}
	// 旧消息和本次用户消息都必须在API中呈现空数组。
	for _, i := range []int{0, 1, 2} {
		if messages[i].Citations == nil || len(messages[i].Citations) != 0 {
			t.Fatalf("message %d must expose an empty citation array", i)
		}
	}
}

func TestMessageHandlerRAGFlow(t *testing.T) {
	for _, mode := range []string{"ordinary", "stream"} {
		for _, outcome := range []string{"answer", "no_evidence"} {
			t.Run(mode+"/"+outcome, func(t *testing.T) {
				h := newRAGHTTPHarness()
				wantContent := "1. 虚构测试陈述 [R1]\n\n依据（检索时快照）：\n" +
					"[R1] 规则：\"虚构规则\"；条款：\"TEST-1\"；版本：\"test-v1\"\n" +
					"来源：\"https://www.bilibili.com/__rag_fixture__\""
				wantCitations := ragHTTPExpectedCitations()
				wantGenerate := 1
				if outcome == "no_evidence" {
					h.retrieved.Items = []retrieval.HybridItem{}
					h.retrieved.Outcome = retrieval.HybridNoEvidence
					h.retrieved.EmptyReason = "no_candidates"
					h.retrieved.Stats.FinalCount = 0
					wantContent = "当前知识库未找到足够依据回答这个问题。请补充规则名称或具体场景；这不表示平台没有相关规则。"
					wantCitations = model.MessageCitations{}
					wantGenerate = 0
				}

				router, token := h.router(t)
				recorder := httptest.NewRecorder()
				h.store.before = func(context.Context) error {
					if mode == "stream" {
						// CreatePair尚未完成，不能提前发送引用或done。
						ragHTTPFrames(t, recorder, "start", "token")
					} else if recorder.Body.Len() != 0 {
						t.Fatal("ordinary response was written before saving")
					}
					return nil
				}
				path := ragHTTPMessagePath
				if mode == "stream" {
					path += "/stream"
				}
				router.ServeHTTP(recorder, ragHTTPRequest(
					context.Background(), token, http.MethodPost, path,
				))
				if h.retrieveCalls != 1 || h.generateCalls != wantGenerate ||
					h.store.attempts != 1 || h.store.reply == nil ||
					h.store.reply.Content != wantContent ||
					!reflect.DeepEqual(h.store.reply.Citations, wantCitations) {
					t.Fatalf("unexpected pipeline/save result: %+v", h.store.reply)
				}

				if mode == "ordinary" {
					if recorder.Code != http.StatusCreated {
						t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
					}
					var user model.Message
					if err := json.Unmarshal(recorder.Body.Bytes(), &user); err != nil {
						t.Fatal(err)
					}
					if user.ID != 10 || user.Role != "user" ||
						user.Content != "测试问题" ||
						user.Citations == nil || len(user.Citations) != 0 {
						t.Fatalf("ordinary response changed: %+v", user)
					}
				} else {
					if recorder.Code != http.StatusOK ||
						recorder.Header().Get("Content-Type") != "text/event-stream; charset=utf-8" ||
						recorder.Header().Get("Cache-Control") != "no-cache, no-transform" ||
						recorder.Header().Get("X-Accel-Buffering") != "no" ||
						!recorder.Flushed {
						t.Fatal("unexpected SSE status/headers/flush")
					}
					frames := ragHTTPFrames(t, recorder, "start", "token", "citation", "done")
					var start struct {
						ConversationID uint64 `json:"conversation_id"`
					}
					var body struct {
						Content string `json:"content"`
					}
					var citations struct {
						Items model.MessageCitations `json:"items"`
					}
					var done struct {
						UserID      uint64 `json:"user_message_id"`
						AssistantID uint64 `json:"assistant_message_id"`
					}
					for i, target := range []any{&start, &body, &citations, &done} {
						if err := json.Unmarshal(frames[i], target); err != nil {
							t.Fatal(err)
						}
					}
					if start.ConversationID != 1 ||
						body.Content != wantContent ||
						!reflect.DeepEqual(citations.Items, wantCitations) ||
						done.UserID != 10 || done.AssistantID != 11 {
						t.Fatal("SSE content/citations/IDs differ from saved messages")
					}
					if strings.Contains(recorder.Body.String(), `"statements"`) {
						t.Fatal("raw model JSON was forwarded to the client")
					}
				}
				ragHTTPHistory(t, router, token, wantContent, wantCitations)
			})
		}
	}
}

func TestMessageHandlerRAGErrors(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		code      string
		message   string
		retrieves int
		generates int
		saves     int
	}{
		{
			"retrieval", 503, "rag_retrieval_unavailable",
			"规则检索暂时不可用，请稍后再试", 1, 0, 0,
		},
		{
			"generation", 502, "rag_generation_failed",
			"回答生成失败，请稍后再试", 1, 1, 0,
		},
		{
			"invalid_output", 502, "rag_invalid_answer",
			"生成结果未通过校验，请稍后再试", 1, 1, 0,
		},
		{
			"unfinished_output", 502, "rag_invalid_answer",
			"生成结果未通过校验，请稍后再试", 1, 1, 0,
		},
		{
			"work_deadline", 504, "request_timeout",
			"request timed out", 0, 0, 0,
		},
		{
			"save", 500, "internal_error",
			"internal server error", 1, 1, 1,
		},
	}

	for _, tc := range cases {
		for _, mode := range []string{"ordinary", "stream"} {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				h := newRAGHTTPHarness()
				ctx := context.Background()
				switch tc.name {
				case "retrieval":
					h.retrieveErr = errors.New("PRIVATE_RETRIEVAL_ERROR")
				case "generation":
					h.generateErr = errors.New("PRIVATE_GENERATION_ERROR")
				case "invalid_output":
					h.raw.Content = "PRIVATE_INVALID_OUTPUT"
				case "unfinished_output":
					h.raw.ResponseMeta.FinishReason = "length"
				case "work_deadline":
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, 3*time.Second)
					defer cancel()
				case "save":
					h.store.saveErr = errors.New("PRIVATE_SAVE_ERROR")
				}
				router, token := h.router(t)
				path := ragHTTPMessagePath
				if mode == "stream" {
					path += "/stream"
				}
				recorder := httptest.NewRecorder()
				router.ServeHTTP(recorder, ragHTTPRequest(ctx, token, http.MethodPost, path))

				var payload []byte
				if mode == "ordinary" {
					if recorder.Code != tc.status {
						t.Fatalf("status=%d want=%d body=%s",
							recorder.Code, tc.status, recorder.Body.String())
					}
					payload = recorder.Body.Bytes()
				} else {
					// start已发送，HTTP状态不能再改；错误通过SSE事件表达。
					if recorder.Code != http.StatusOK {
						t.Fatalf("started stream status=%d", recorder.Code)
					}
					if tc.name == "save" {
						frames := ragHTTPFrames(t, recorder, "start", "token", "error")
						payload = frames[2]
					} else {
						frames := ragHTTPFrames(t, recorder, "start", "error")
						payload = frames[1]
					}
				}

				var response ErrorResponse
				if err := json.Unmarshal(payload, &response); err != nil {
					t.Fatal(err)
				}
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(payload, &fields); err != nil {
					t.Fatal(err)
				}
				if len(fields) != 2 ||
					response.Code != tc.code ||
					response.Message != tc.message ||
					strings.Contains(string(payload), "PRIVATE_") {
					t.Fatalf("unexpected public error: %s", payload)
				}
				if h.retrieveCalls != tc.retrieves ||
					h.generateCalls != tc.generates ||
					h.store.attempts != tc.saves ||
					h.store.created != nil ||
					h.store.reply != nil ||
					len(h.store.messages) != 2 {
					t.Fatal("failed request changed message history or call counts")
				}
			})
		}
	}
}

func TestMessageHandlerRAGCanceledAfterSave(t *testing.T) {
	h := newRAGHTTPHarness()
	router, token := h.router(t)
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	recorder := httptest.NewRecorder()

	h.store.before = func(saveCtx context.Context) error {
		ragHTTPFrames(t, recorder, "start", "token")
		cancel()
		if saveCtx.Err() != nil {
			t.Fatalf("save context must survive parent cancellation: %v", saveCtx.Err())
		}
		return nil
	}
	router.ServeHTTP(recorder, ragHTTPRequest(
		parent, token, http.MethodPost, ragHTTPMessagePath+"/stream",
	))
	if h.store.attempts != 1 || h.store.reply == nil ||
		!errors.Is(parent.Err(), context.Canceled) {
		t.Fatal("expected a saved pair after handoff cancellation")
	}
	// 保存完成，但请求已经取消，不再发送citation/done。
	ragHTTPFrames(t, recorder, "start", "token")
	ragHTTPHistory(
		t, router, token, h.store.reply.Content, ragHTTPExpectedCitations(),
	)
}
