package service

import (
	"context"
	"errors"
	"testing"

	"video-support-agent/internal/cache"
	"video-support-agent/internal/model"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

type messageContextStub struct {
	called         bool
	conversationID uint64
	message        cache.ContextMessage
	hasDeadline    bool
	err            error
	received       []cache.ContextMessage
}

func (s *messageContextStub) Append(
	ctx context.Context,
	conversationID uint64,
	message cache.ContextMessage,
) error {
	s.called = true
	s.conversationID = conversationID
	s.message = message
	s.received = append(s.received, message)
	_, s.hasDeadline = ctx.Deadline()
	return s.err
}

func TestMessageServiceContextWrite(t *testing.T) {
	dbErr := errors.New("mysql unavailable")
	cacheErr := errors.New("redis unavailable")

	tests := []struct {
		name          string
		ownerID       uint64
		dbErr         error
		cacheErr      error
		wantErr       error
		wantCacheCall bool
		wantWarning   bool
	}{
		{
			name:          "save then cache",
			ownerID:       7,
			wantCacheCall: true,
		},
		{
			name:          "cache failure preserves success",
			ownerID:       7,
			cacheErr:      cacheErr,
			wantCacheCall: true,
			wantWarning:   true,
		},
		{
			name:    "database failure skips cache",
			ownerID: 7,
			dbErr:   dbErr,
			wantErr: dbErr,
		},
		{
			name:    "unauthorized owner skips cache",
			ownerID: 8,
			wantErr: ErrConversationNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			messages := &fakeMessageStore{err: tt.dbErr}
			conversations := &fakeConversationStore{
				found: &model.Conversation{
					ID:     1,
					UserID: tt.ownerID,
				},
			}
			contexts := &messageContextStub{err: tt.cacheErr}
			core, logs := observer.New(zap.WarnLevel)

			svc := NewMessageService(
				messages,
				conversations,
				contexts,
				zap.New(core),
			)

			got, err := svc.Create(
				context.Background(),
				7,
				1,
				"  测试消息  ",
			)

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("expected %v, got %v", tt.wantErr, err)
				}
				if got != nil {
					t.Fatal("failed operation must not return a message")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if got == nil || got.ID != 10 {
					t.Fatal("expected the message saved by MySQL")
				}
			}

			if contexts.called != tt.wantCacheCall {
				t.Fatalf(
					"expected cache call %v, got %v",
					tt.wantCacheCall,
					contexts.called,
				)
			}

			if contexts.called {
				if contexts.conversationID != 1 || len(contexts.received) != 2 {
					t.Fatal("cache must receive both saved messages")
				}

				first := contexts.received[0]
				second := contexts.received[1]

				if first.ID != 10 ||
					first.Role != "user" ||
					first.Content != "测试消息" {
					t.Fatal("first cached message must be the user message")
				}

				if second.ID != 11 ||
					second.Role != "assistant" ||
					second.Content != demoReply {
					t.Fatal("second cached message must be the assistant reply")
				}
				if !contexts.hasDeadline {
					t.Fatal("cache update must have a deadline")
				}
			}

			warnings := logs.FilterMessage(
				"update conversation context failed",
			).Len()

			wantWarnings := 0
			if tt.wantWarning {
				wantWarnings = 2
			}
			if warnings != wantWarnings {
				t.Fatalf(
					"expected %d warnings, got %d",
					wantWarnings,
					warnings,
				)
			}
		})
	}
}

func TestMessageServiceHistoryStillUsesMySQL(t *testing.T) {
	messages := &fakeMessageStore{
		messages: []model.Message{
			{
				ID:             10,
				ConversationID: 1,
				Role:           "user",
				Content:        "MySQL 历史消息",
			},
		},
	}
	conversations := &fakeConversationStore{
		found: &model.Conversation{ID: 1, UserID: 7},
	}
	contexts := &messageContextStub{
		err: errors.New("redis unavailable"),
	}

	svc := NewMessageService(messages, conversations, contexts, nil)

	got, err := svc.ListByConversationID(context.Background(), 7, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !messages.listed || len(got) != 1 || got[0].ID != 10 {
		t.Fatal("history must come from MySQL")
	}
	if contexts.called {
		t.Fatal("reading history must not append to Redis")
	}
}
