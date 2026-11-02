package service

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"video-support-agent/internal/llm"
	"video-support-agent/internal/model"
	"video-support-agent/internal/rag"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

type groundedMessageStub struct {
	reply         model.GroundedReply
	err           error
	onReply       func(context.Context) (model.GroundedReply, error)
	groundedCalls int
	legacyCalls   int
	streamCalls   int
	history       []llm.HistoryMessage
	question      string
}

func (s *groundedMessageStub) Reply(
	context.Context, []llm.HistoryMessage, string,
) (string, error) {
	s.legacyCalls++
	return "不应使用的旧接口回答", nil
}

func (s *groundedMessageStub) ReplyGrounded(
	ctx context.Context,
	history []llm.HistoryMessage,
	question string,
) (model.GroundedReply, error) {
	s.groundedCalls++
	s.history = append([]llm.HistoryMessage(nil), history...)
	s.question = question
	if s.onReply != nil {
		return s.onReply(ctx)
	}
	return s.reply, s.err
}

type groundedWithLegacyStream struct {
	*groundedMessageStub
}

func (s *groundedWithLegacyStream) ReplyStream(
	context.Context,
	[]llm.HistoryMessage,
	string,
	func(string) error,
) (string, error) {
	s.streamCalls++
	return "不应使用的旧流式回答", nil
}

type groundedMessageStore struct {
	fakeMessageStore
	attempts int
	saveCtx  context.Context
	before   func(context.Context, *model.Message, *model.Message) error
}

func (s *groundedMessageStore) CreatePair(
	ctx context.Context,
	user, assistant *model.Message,
) error {
	s.attempts++
	s.saveCtx = ctx
	if s.before != nil {
		if err := s.before(ctx, user, assistant); err != nil {
			return err
		}
	}
	return s.fakeMessageStore.CreatePair(ctx, user, assistant)
}

// 明确虚构的内存快照，不写真实知识库。
func groundedMessageFixture(decision model.ReplyDecision) model.GroundedReply {
	at := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	expiry := at.Add(time.Hour)
	reply := model.GroundedReply{
		Decision: decision,
		Content:  "  已校验的虚构回答\n保留末尾空格  ",
		Citations: []model.RuleCitation{
			{
				RefID:         "R1",
				DocumentID:    "9007199254740993",
				VersionID:     "9007199254740995",
				ArticleID:     "9007199254740997",
				CategoryID:    "1",
				ChunkID:       "501",
				ChunkIDs:      []string{"501", "502"},
				Version:       "test-v1",
				Title:         "虚构规则",
				SectionPath:   "测试章节",
				ArticleNumber: "TEST-1",
				SourceURL:     "https://www.bilibili.com/__rag_fixture__",
				EffectiveAt:   at.Add(-time.Hour),
				ExpiresAt:     &expiry,
				RetrievedAt:   at,
			},
		},
	}
	if decision != model.ReplyAnswer {
		reply.Citations = []model.RuleCitation{}
	}
	return reply
}

func groundedTestService(
	store MessageStore,
	contexts MessageContextStore,
	chat ChatResponder,
	logger *zap.Logger,
) *MessageService {
	return NewMessageService(
		store,
		&fakeConversationStore{
			found: &model.Conversation{ID: 1, UserID: 7},
		},
		contexts,
		logger,
		chat,
	)
}

func requireGroundedOnly(t *testing.T, chat *groundedMessageStub, calls int) {
	t.Helper()
	if chat.groundedCalls != calls ||
		chat.legacyCalls != 0 ||
		chat.streamCalls != 0 {
		t.Fatalf(
			"grounded=%d legacy=%d stream=%d; want %d/0/0",
			chat.groundedCalls, chat.legacyCalls, chat.streamCalls, calls,
		)
	}
}

func requireGroundedGuardReleased(t *testing.T, svc *MessageService) {
	t.Helper()
	release, ok := svc.guard.TryAcquire(1)
	if !ok {
		t.Fatal("conversation guard was not released")
	}
	release()
}

func TestMessageServiceGroundedCreate(t *testing.T) {
	for _, decision := range []model.ReplyDecision{
		model.ReplyAnswer,
		model.ReplyClarify,
		model.ReplyHandoff,
		model.ReplyInsufficient,
	} {
		t.Run(string(decision), func(t *testing.T) {
			store := &groundedMessageStore{
				fakeMessageStore: fakeMessageStore{
					messages: []model.Message{
						{ID: 1, ConversationID: 1, Role: "user", Content: "旧问题"},
						{ID: 2, ConversationID: 1, Role: "assistant", Content: "旧回答"},
					},
				},
			}
			contexts := &messageContextStub{}
			chat := &groundedMessageStub{reply: groundedMessageFixture(decision)}
			wantCitations := model.NewMessageCitations(chat.reply.Citations)
			parent := context.Background()

			store.before = func(
				ctx context.Context, user, assistant *model.Message,
			) error {
				if ctx != parent {
					t.Fatal("ordinary Create must use the request context")
				}
				if user.Role != "user" || user.Content != "本次问题" ||
					user.ConversationID != 1 || len(user.Citations) != 0 {
					t.Fatalf("unexpected user message: %+v", user)
				}
				if assistant.Role != "assistant" ||
					assistant.ConversationID != 1 ||
					assistant.Content != chat.reply.Content ||
					!reflect.DeepEqual(assistant.Citations, wantCitations) {
					t.Fatalf("unexpected assistant message: %+v", assistant)
				}
				return nil
			}

			svc := groundedTestService(
				store, contexts, &groundedWithLegacyStream{chat}, nil,
			)
			got, err := svc.Create(parent, 7, 1, "  本次问题  ")
			if err != nil {
				t.Fatal(err)
			}
			if got == nil || got != store.created ||
				got.ID != 10 || got.Role != "user" {
				t.Fatalf("Create must return the saved user message: %+v", got)
			}
			requireGroundedOnly(t, chat, 1)
			if chat.question != "本次问题" ||
				!reflect.DeepEqual(chat.history, []llm.HistoryMessage{
					{Role: "user", Content: "旧问题"},
					{Role: "assistant", Content: "旧回答"},
				}) {
				t.Fatalf("question=%q history=%+v", chat.question, chat.history)
			}
			if store.attempts != 1 || store.saveCalls != 1 ||
				store.reply == nil || store.reply.ID != 11 {
				t.Fatal("the message pair must be saved exactly once")
			}
			if !contexts.called || len(contexts.received) != 4 ||
				contexts.received[3].Content != chat.reply.Content {
				t.Fatalf("unexpected refreshed context: %+v", contexts.received)
			}

			if decision == model.ReplyAnswer {
				chat.reply.Citations[0].Title = "修改调用方快照"
				chat.reply.Citations[0].ChunkIDs[0] = "999"
				*chat.reply.Citations[0].ExpiresAt =
					chat.reply.Citations[0].ExpiresAt.Add(time.Hour)

				if !reflect.DeepEqual(store.reply.Citations, wantCitations) {
					t.Fatal("saved citations alias the responder's mutable snapshot")
				}
			}
			requireGroundedGuardReleased(t, svc)
		})
	}
}

func TestMessageServiceGroundedRejects(t *testing.T) {
	cases := []struct {
		name   string
		change func(*groundedMessageStub)
		want   error
	}{
		{"blank_content", func(s *groundedMessageStub) {
			s.reply.Content = " \n "
		}, rag.ErrInvalidAnswer},
		{"invalid_content_encoding", func(s *groundedMessageStub) {
			s.reply.Content = string([]byte{0xff})
		}, rag.ErrInvalidAnswer},
		{"unknown_decision", func(s *groundedMessageStub) {
			s.reply.Decision = "unknown"
		}, rag.ErrInvalidAnswer},
		{"answer_without_citations", func(s *groundedMessageStub) {
			s.reply.Citations = nil
		}, rag.ErrInvalidAnswer},
		{"too_many_citations", func(s *groundedMessageStub) {
			for len(s.reply.Citations) < 6 {
				s.reply.Citations = append(s.reply.Citations, s.reply.Citations[0])
			}
		}, rag.ErrInvalidAnswer},
		{"clarify_with_citation", func(s *groundedMessageStub) {
			s.reply.Decision = model.ReplyClarify
		}, rag.ErrInvalidAnswer},
		{"handoff_with_citation", func(s *groundedMessageStub) {
			s.reply.Decision = model.ReplyHandoff
		}, rag.ErrInvalidAnswer},
		{"insufficient_with_citation", func(s *groundedMessageStub) {
			s.reply.Decision = model.ReplyInsufficient
		}, rag.ErrInvalidAnswer},
		{"citation_encoding", func(s *groundedMessageStub) {
			s.reply.Citations[0].Title = string([]byte{0xff})
		}, rag.ErrInvalidAnswer},
		{"citation_json_budget", func(s *groundedMessageStub) {
			s.reply.Citations[0].Title = strings.Repeat("X", 128<<10)
		}, rag.ErrInvalidAnswer},
		{"retrieval_error", func(s *groundedMessageStub) {
			s.err = rag.ErrRetrievalFailed
		}, rag.ErrRetrievalFailed},
		{"generation_error", func(s *groundedMessageStub) {
			s.err = rag.ErrGenerationFailed
		}, rag.ErrGenerationFailed},
		{"invalid_answer_error", func(s *groundedMessageStub) {
			s.err = rag.ErrInvalidAnswer
		}, rag.ErrInvalidAnswer},
		{"work_deadline", func(s *groundedMessageStub) {
			s.err = context.DeadlineExceeded
		}, context.DeadlineExceeded},
	}

	for _, tc := range cases {
		for _, mode := range []string{"ordinary", "stream"} {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				store := &groundedMessageStore{}
				contexts := &messageContextStub{}
				chat := &groundedMessageStub{
					reply: groundedMessageFixture(model.ReplyAnswer),
				}
				tc.change(chat)
				svc := groundedTestService(
					store, contexts, &groundedWithLegacyStream{chat}, nil,
				)
				starts, chunks := 0, 0
				var err error
				if mode == "ordinary" {
					var got *model.Message
					got, err = svc.Create(context.Background(), 7, 1, "测试问题")
					if got != nil {
						t.Fatal("failure returned a user message")
					}
				} else {
					var got *StreamResult
					got, err = svc.CreateStream(
						context.Background(), 7, 1, "测试问题",
						StreamCallbacks{
							OnStart: func() error { starts++; return nil },
							OnChunk: func(string) error { chunks++; return nil },
						},
					)
					if got != nil || starts != 1 || chunks != 0 {
						t.Fatalf("result=%+v starts=%d chunks=%d", got, starts, chunks)
					}
				}
				if !errors.Is(err, tc.want) {
					t.Fatalf("error=%v want=%v", err, tc.want)
				}
				requireGroundedOnly(t, chat, 1)
				if store.attempts != 0 || contexts.called {
					t.Fatal("rejected reply must not save or refresh cache")
				}
				requireGroundedGuardReleased(t, svc)
			})
		}
	}
}

func TestMessageServiceGroundedCancellation(t *testing.T) {
	for _, mode := range []string{"ordinary", "stream"} {
		for _, point := range []string{"before", "during", "during_with_error"} {
			t.Run(mode+"/"+point, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				store := &groundedMessageStore{}
				contexts := &messageContextStub{}
				chat := &groundedMessageStub{
					reply: groundedMessageFixture(model.ReplyAnswer),
				}
				wantCalls := 1
				if point == "before" {
					cancel()
					wantCalls = 0
				} else {
					chat.onReply = func(context.Context) (model.GroundedReply, error) {
						cancel()
						if point == "during_with_error" {
							return chat.reply, rag.ErrGenerationFailed
						}
						return chat.reply, nil
					}
				}
				svc := groundedTestService(store, contexts, chat, nil)
				chunks := 0
				var err error
				if mode == "ordinary" {
					var got *model.Message
					got, err = svc.Create(ctx, 7, 1, "测试问题")
					if got != nil {
						t.Fatal("canceled Create returned a message")
					}
				} else {
					var got *StreamResult
					got, err = svc.CreateStream(ctx, 7, 1, "测试问题", StreamCallbacks{
						OnStart: func() error { return nil },
						OnChunk: func(string) error { chunks++; return nil },
					})
					if got != nil {
						t.Fatal("canceled stream returned a result")
					}
				}
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("parent cancellation must win: %v", err)
				}
				requireGroundedOnly(t, chat, wantCalls)
				if store.attempts != 0 || chunks != 0 || contexts.called {
					t.Fatal("canceled generation emitted or saved a reply")
				}
				requireGroundedGuardReleased(t, svc)
			})
		}
	}
}

func TestMessageServiceGroundedStreamOrder(t *testing.T) {
	for _, capability := range []string{"grounded_only", "also_legacy_stream"} {
		for _, decision := range []model.ReplyDecision{
			model.ReplyAnswer, model.ReplyClarify,
			model.ReplyHandoff, model.ReplyInsufficient,
		} {
			t.Run(capability+"/"+string(decision), func(t *testing.T) {
				store := &groundedMessageStore{}
				contexts := &messageContextStub{}
				chat := &groundedMessageStub{reply: groundedMessageFixture(decision)}
				var events []string
				chat.onReply = func(context.Context) (model.GroundedReply, error) {
					events = append(events, "generate")
					return chat.reply, nil
				}
				store.before = func(
					context.Context, *model.Message, *model.Message,
				) error {
					events = append(events, "save")
					return nil
				}
				var responder ChatResponder = chat
				if capability == "also_legacy_stream" {
					responder = &groundedWithLegacyStream{chat}
				}
				svc := groundedTestService(store, contexts, responder, nil)
				result, err := svc.CreateStream(
					context.Background(), 7, 1, "  本次问题  ",
					StreamCallbacks{
						OnStart: func() error {
							events = append(events, "start")
							return nil
						},
						OnChunk: func(part string) error {
							if part != chat.reply.Content || store.attempts != 0 {
								t.Fatal("must hand off the complete answer before saving")
							}
							events = append(events, "chunk")
							return nil
						},
					},
				)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(events, []string{"start", "generate", "chunk", "save"}) {
					t.Fatalf("unexpected event order: %v", events)
				}
				requireGroundedOnly(t, chat, 1)
				if result == nil || result.UserMessage == nil ||
					result.AssistantMessage == nil ||
					result.UserMessage.ID != 10 ||
					result.AssistantMessage.ID != 11 ||
					result.AssistantMessage.Content != chat.reply.Content ||
					!reflect.DeepEqual(
						result.AssistantMessage.Citations,
						model.NewMessageCitations(chat.reply.Citations),
					) {
					t.Fatalf("unexpected saved result: %+v", result)
				}
				if store.attempts != 1 || !contexts.called ||
					len(contexts.received) != 2 {
					t.Fatal("save/cache refresh contract was not preserved")
				}
				requireGroundedGuardReleased(t, svc)
			})
		}
	}
}

func TestMessageServiceGroundedStreamFailures(t *testing.T) {
	for _, point := range []string{"start", "chunk", "save"} {
		t.Run(point, func(t *testing.T) {
			failure := errors.New("PRIVATE_FAILURE")
			store := &groundedMessageStore{}
			contexts := &messageContextStub{}
			chat := &groundedMessageStub{
				reply: groundedMessageFixture(model.ReplyAnswer),
			}
			if point == "save" {
				store.saveErr = failure
			}
			core, logs := observer.New(zap.DebugLevel)
			svc := groundedTestService(
				store, contexts, &groundedWithLegacyStream{chat}, zap.New(core),
			)
			starts, chunks := 0, 0
			result, err := svc.CreateStream(
				context.Background(), 7, 1, "PRIVATE_QUESTION",
				StreamCallbacks{
					OnStart: func() error {
						starts++
						if point == "start" {
							return failure
						}
						return nil
					},
					OnChunk: func(string) error {
						chunks++
						if point == "chunk" {
							return failure
						}
						return nil
					},
				},
			)
			if result != nil || !errors.Is(err, failure) {
				t.Fatalf("result=%+v error=%v", result, err)
			}
			wantCalls, wantChunks, wantSaves := 1, 1, 0
			stage := "generate"
			if point == "start" {
				wantCalls, wantChunks, stage = 0, 0, "start"
			}
			if point == "save" {
				wantSaves, stage = 1, "save"
			}
			requireGroundedOnly(t, chat, wantCalls)
			if starts != 1 || chunks != wantChunks ||
				store.attempts != wantSaves || contexts.called ||
				store.created != nil || store.reply != nil {
				t.Fatal("failure emitted an unexpected save/cache/result")
			}
			entries := logs.FilterMessage("chat stream finished").All()
			if len(entries) != 1 {
				t.Fatalf("completion log count=%d", len(entries))
			}
			fields := entries[0].ContextMap()
			if entries[0].Level != zap.WarnLevel ||
				len(fields) != 4 ||
				fields["stage"] != stage ||
				fields["outcome"] != "failed" ||
				fields["saved"] != false {
				t.Fatalf("unexpected completion log: %+v", fields)
			}
			for _, field := range entries[0].Context {
				if strings.Contains(field.String, "PRIVATE_") {
					t.Fatal("private data leaked into completion log")
				}
			}
			if store.saveCtx != nil &&
				!errors.Is(store.saveCtx.Err(), context.Canceled) {
				t.Fatal("save context was not released")
			}
			requireGroundedGuardReleased(t, svc)
		})
	}
}

func TestMessageServiceGroundedStreamHandoff(t *testing.T) {
	type requestKey struct{}
	for _, outcome := range []string{"saved", "save_timeout"} {
		t.Run(outcome, func(t *testing.T) {
			base := context.WithValue(context.Background(), requestKey{}, "request-value")
			parent, cancel := context.WithTimeout(base, time.Second)
			defer cancel()
			parentDeadline, _ := parent.Deadline()

			store := &groundedMessageStore{}
			contexts := &messageContextStub{}
			chat := &groundedMessageStub{
				reply: groundedMessageFixture(model.ReplyAnswer),
			}
			var handedOffAt time.Time
			store.before = func(
				ctx context.Context, user, assistant *model.Message,
			) error {
				if !errors.Is(parent.Err(), context.Canceled) {
					t.Fatal("parent should have been canceled after handoff")
				}
				if ctx.Err() != nil || ctx.Value(requestKey{}) != "request-value" {
					t.Fatal("save context lost usability or request values")
				}
				deadline, ok := ctx.Deadline()
				if !ok ||
					deadline.Before(handedOffAt.Add(3*time.Second)) ||
					deadline.After(time.Now().Add(3*time.Second)) ||
					!deadline.After(parentDeadline) {
					t.Fatalf("unexpected independent save deadline: %v", deadline)
				}
				if assistant.Content != chat.reply.Content ||
					!reflect.DeepEqual(
						assistant.Citations,
						model.NewMessageCitations(chat.reply.Citations),
					) {
					t.Fatal("handoff changed content or citations")
				}
				if outcome == "save_timeout" {
					<-ctx.Done()
					return ctx.Err()
				}
				return nil
			}
			svc := groundedTestService(store, contexts, chat, nil)
			result, err := svc.CreateStream(parent, 7, 1, "测试问题", StreamCallbacks{
				OnStart: func() error { return nil },
				OnChunk: func(string) error {
					handedOffAt = time.Now()
					cancel()
					// 正文交接成功；取消发生在这个边界。
					return nil
				},
			})
			requireGroundedOnly(t, chat, 1)
			if store.attempts != 1 || store.saveCtx == nil {
				t.Fatal("expected one bounded save attempt")
			}
			wantContextErr := context.Canceled
			if outcome == "save_timeout" {
				wantContextErr = context.DeadlineExceeded
				if !errors.Is(err, context.DeadlineExceeded) ||
					result != nil || contexts.called {
					t.Fatalf("timeout result=%+v error=%v", result, err)
				}
			} else if err != nil || result == nil ||
				result.AssistantMessage == nil ||
				result.AssistantMessage.ID != 11 ||
				!contexts.called {
				t.Fatalf("successful handoff result=%+v error=%v", result, err)
			}
			if !errors.Is(store.saveCtx.Err(), wantContextErr) {
				t.Fatalf("save context error=%v", store.saveCtx.Err())
			}
			requireGroundedGuardReleased(t, svc)
		})
	}
}

func TestMessageServiceGroundedStreamGuard(t *testing.T) {
	store := &groundedMessageStore{}
	chat := &groundedMessageStub{
		reply: groundedMessageFixture(model.ReplyAnswer),
	}
	entered := make(chan struct{})
	proceed := make(chan struct{})
	chat.onReply = func(ctx context.Context) (model.GroundedReply, error) {
		if chat.groundedCalls == 1 {
			close(entered)
			select {
			case <-proceed:
			case <-ctx.Done():
				return model.GroundedReply{}, ctx.Err()
			}
		}
		return chat.reply, nil
	}
	svc := groundedTestService(store, nil, chat, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	type completion struct {
		result *StreamResult
		err    error
	}
	done := make(chan completion, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		result, err := svc.CreateStream(ctx, 7, 1, "第一条", StreamCallbacks{
			OnStart: func() error { return nil },
			OnChunk: func(string) error { return nil },
		})
		done <- completion{result: result, err: err}
	}()
	defer func() {
		cancel()
		select {
		case <-exited:
		case <-time.After(2 * time.Second):
			t.Error("controlled request did not exit")
		}
	}()

	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("first request did not enter generation")
	}

	if got, err := svc.Create(ctx, 7, 1, "第二条"); got != nil ||
		!errors.Is(err, ErrConversationBusy) {
		t.Fatalf("ordinary concurrent request: result=%+v error=%v", got, err)
	}
	started := false
	if got, err := svc.CreateStream(ctx, 7, 1, "第三条", StreamCallbacks{
		OnStart: func() error { started = true; return nil },
		OnChunk: func(string) error { return nil },
	}); got != nil || !errors.Is(err, ErrConversationBusy) || started {
		t.Fatalf("concurrent stream: result=%+v error=%v started=%v", got, err, started)
	}

	close(proceed)
	select {
	case first := <-done:
		if first.err != nil || first.result == nil {
			t.Fatalf("first request: %+v", first)
		}
	case <-ctx.Done():
		t.Fatal("first request did not complete")
	}
	requireGroundedOnly(t, chat, 1)
	if store.attempts != 1 {
		t.Fatalf("save attempts=%d", store.attempts)
	}

	if _, err := svc.Create(ctx, 7, 1, "释放后请求"); err != nil {
		t.Fatalf("conversation was not reusable: %v", err)
	}
	requireGroundedOnly(t, chat, 2)
	if store.attempts != 2 {
		t.Fatalf("save attempts=%d", store.attempts)
	}
}
