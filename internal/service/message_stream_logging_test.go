package service

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"video-support-agent/internal/llm"
	"video-support-agent/internal/model"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func TestMessageServiceStreamFinishedLog(t *testing.T) {
	dbErr := errors.New("private-database-error-marker")

	tests := []struct {
		name               string
		modelErr           error
		saveErr            error
		cancelDuring       bool
		cancelAfterSuccess bool
		wantErr            error
		wantStage          string
		wantOutcome        string
		wantSaved          bool
		wantSaveCalls      int
		wantLevel          zapcore.Level
	}{
		{
			name:          "success",
			wantStage:     "completed",
			wantOutcome:   "success",
			wantSaved:     true,
			wantSaveCalls: 1,
			wantLevel:     zap.InfoLevel,
		},
		{
			name: "generation failure",
			modelErr: fmt.Errorf(
				"private-model-error-marker: %w",
				llm.ErrModelFailed,
			),
			wantErr:     llm.ErrModelFailed,
			wantStage:   "generate",
			wantOutcome: "failed",
			wantLevel:   zap.WarnLevel,
		},
		{
			name:        "model timeout",
			modelErr:    llm.ErrModelTimeout,
			wantErr:     llm.ErrModelTimeout,
			wantStage:   "generate",
			wantOutcome: "timeout",
			wantLevel:   zap.WarnLevel,
		},
		{
			name:         "cancel during generation",
			cancelDuring: true,
			wantErr:      context.Canceled,
			wantStage:    "generate",
			wantOutcome:  "canceled",
			wantLevel:    zap.InfoLevel,
		},
		{
			name:          "save failure after generation",
			saveErr:       dbErr,
			wantErr:       dbErr,
			wantStage:     "save",
			wantOutcome:   "failed",
			wantSaveCalls: 1,
			wantLevel:     zap.WarnLevel,
		},
		{
			name:               "save succeeds after request cancellation",
			cancelAfterSuccess: true,
			wantStage:          "completed",
			wantOutcome:        "success",
			wantSaved:          true,
			wantSaveCalls:      1,
			wantLevel:          zap.InfoLevel,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			core, logs := observer.New(zap.DebugLevel)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			store := &streamSaveStore{
				fakeMessageStore: fakeMessageStore{
					saveErr: tt.saveErr,
				},
				onSave: func(saveCtx context.Context) error {
					if tt.cancelAfterSuccess &&
						!errors.Is(ctx.Err(), context.Canceled) {
						t.Fatal("request should already be canceled")
					}

					if err := saveCtx.Err(); err != nil {
						t.Fatalf("save context must remain usable: %v", err)
					}

					if _, ok := saveCtx.Deadline(); !ok {
						t.Fatal("save context must have a deadline")
					}

					return nil
				},
			}

			conversations := &fakeConversationStore{
				found: &model.Conversation{ID: 1, UserID: 7},
			}

			chat := &streamChatStub{
				replyStream: func(
					modelCtx context.Context,
					_ []llm.HistoryMessage,
					_ string,
					onChunk func(string) error,
				) (string, error) {
					if err := onChunk("private-answer-marker"); err != nil {
						return "", err
					}

					if tt.cancelDuring {
						cancel()
						return "", modelCtx.Err()
					}

					if tt.modelErr != nil {
						return "", tt.modelErr
					}

					if tt.cancelAfterSuccess {
						// 模拟完整生成成功后，交接保存时父请求取消。
						cancel()
					}

					return "private-answer-marker", nil
				},
			}

			svc := NewMessageService(
				store,
				conversations,
				nil,
				zap.New(core),
				chat,
			)

			result, err := svc.CreateStream(
				ctx,
				7,
				1,
				"private-question-marker",
				StreamCallbacks{
					OnStart: func() error { return nil },
					OnChunk: func(string) error { return nil },
				},
			)

			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("expected error %v, got %v", tt.wantErr, err)
			}
			if store.saveCalls != tt.wantSaveCalls {
				t.Fatalf(
					"expected %d save calls, got %d",
					tt.wantSaveCalls,
					store.saveCalls,
				)
			}

			if tt.wantSaved {
				if result == nil ||
					result.UserMessage == nil ||
					result.AssistantMessage == nil {
					t.Fatal("expected both saved messages")
				}
				if result.UserMessage.ID != 10 ||
					result.AssistantMessage.ID != 11 {
					t.Fatal("expected database-assigned message IDs")
				}
			} else if result != nil {
				t.Fatal("failed call must not return a successful result")
			}

			entries := logs.All()
			if len(entries) != 1 {
				t.Fatalf("expected one service log, got %d", len(entries))
			}

			entry := entries[0]
			if entry.Message != "chat stream finished" {
				t.Fatal("unexpected log message")
			}
			if entry.Level != tt.wantLevel {
				t.Fatalf("expected level %v, got %v", tt.wantLevel, entry.Level)
			}

			fields := entry.ContextMap()
			if len(fields) != 4 {
				t.Fatal("only stage, outcome, saved and duration are allowed")
			}
			if fields["stage"] != tt.wantStage {
				t.Fatalf("unexpected stage: %v", fields["stage"])
			}
			if fields["outcome"] != tt.wantOutcome {
				t.Fatalf("unexpected outcome: %v", fields["outcome"])
			}
			if fields["saved"] != tt.wantSaved {
				t.Fatalf("unexpected saved flag: %v", fields["saved"])
			}

			duration, ok := fields["duration"].(time.Duration)
			if !ok || duration < 0 {
				t.Fatal("duration must be a non-negative time.Duration")
			}
		})
	}
}

func TestMessageServiceStreamCacheFailurePreservesSuccessLog(t *testing.T) {
	core, logs := observer.New(zap.DebugLevel)

	messages := &fakeMessageStore{}
	conversations := &fakeConversationStore{
		found: &model.Conversation{ID: 1, UserID: 7},
	}
	contexts := &messageContextStub{
		err: errors.New("private-cache-error-marker"),
	}

	const question = "private-question-marker"
	const answer = "private-answer-marker"

	chat := &streamChatStub{
		replyStream: func(
			_ context.Context,
			_ []llm.HistoryMessage,
			_ string,
			onChunk func(string) error,
		) (string, error) {
			if err := onChunk(answer); err != nil {
				return "", err
			}
			return answer, nil
		},
	}

	svc := NewMessageService(
		messages,
		conversations,
		contexts,
		zap.New(core),
		chat,
	)

	result, err := svc.CreateStream(
		context.Background(),
		7,
		1,
		question,
		StreamCallbacks{
			OnStart: func() error { return nil },
			OnChunk: func(string) error { return nil },
		},
	)

	if err != nil {
		t.Fatalf("cache failure must preserve save success: %v", err)
	}
	if result == nil ||
		result.UserMessage == nil ||
		result.AssistantMessage == nil {
		t.Fatal("expected both saved messages")
	}
	if result.UserMessage.ID != 10 ||
		result.AssistantMessage.ID != 11 {
		t.Fatal("expected saved message IDs")
	}
	if result.UserMessage.Content != question ||
		result.AssistantMessage.Content != answer {
		t.Fatal("saved content changed")
	}
	if messages.saveCalls != 1 || len(messages.messages) != 2 {
		t.Fatal("the message pair must be saved exactly once")
	}

	if !contexts.called {
		t.Fatal("cache refresh must have been attempted")
	}
	if !contexts.hasDeadline {
		t.Fatal("cache refresh must have a deadline")
	}
	if len(contexts.received) != 2 {
		t.Fatal("cache refresh must receive the saved pair")
	}

	entries := logs.All()
	if len(entries) != 2 {
		t.Fatalf("expected two log entries, got %d", len(entries))
	}

	warning := entries[0]
	if warning.Message != "update conversation context failed" ||
		warning.Level != zap.WarnLevel {
		t.Fatal("expected a cache warning before the finish log")
	}

	warningFields := warning.ContextMap()
	if len(warningFields) != 1 ||
		warningFields["conversation_id"] != uint64(1) {
		t.Fatal("cache warning must contain only the conversation ID")
	}

	finished := entries[1]
	if finished.Message != "chat stream finished" ||
		finished.Level != zap.InfoLevel {
		t.Fatal("expected a successful service finish log")
	}

	fields := finished.ContextMap()
	if len(fields) != 4 {
		t.Fatal("only stage, outcome, saved and duration are allowed")
	}
	if fields["stage"] != "completed" ||
		fields["outcome"] != "success" ||
		fields["saved"] != true {
		t.Fatal("cache failure must not change the saved success result")
	}

	duration, ok := fields["duration"].(time.Duration)
	if !ok || duration < 0 {
		t.Fatal("duration must be a non-negative time.Duration")
	}

	release, acquired := svc.guard.TryAcquire(1)
	if !acquired {
		t.Fatal("conversation guard must be released")
	}
	release()
}
