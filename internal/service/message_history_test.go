package service

import (
	"context"
	"errors"
	"testing"

	"video-support-agent/internal/cache"
	"video-support-agent/internal/model"
)

func TestMessageServiceLoadHistory(t *testing.T) {
	tests := []struct {
		name        string
		cached      []cache.ContextMessage
		cacheErr    error
		dbErr       error
		wantDBCall  bool
		wantContent string
		wantErr     error
	}{
		{
			name: "cache hit",
			cached: []cache.ContextMessage{
				{
					ID:      1,
					Role:    "user",
					Content: "MySQL 中的历史",
				},
			},
			wantContent: "MySQL 中的历史",
		},
		{
			name:        "empty cache falls back to MySQL",
			wantDBCall:  true,
			wantContent: "MySQL 中的历史",
		},
		{
			name:        "cache failure falls back to MySQL",
			cacheErr:    errors.New("redis unavailable"),
			wantDBCall:  true,
			wantContent: "MySQL 中的历史",
		},
		{
			name:        "cache timeout falls back to MySQL",
			cacheErr:    context.DeadlineExceeded,
			wantDBCall:  true,
			wantContent: "MySQL 中的历史",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			contexts := &messageContextStub{
				history: tt.cached,
				listErr: tt.cacheErr,
			}

			messages := &fakeMessageStore{
				messages: []model.Message{
					{
						ID:             1,
						ConversationID: 1,
						Role:           "user",
						Content:        "MySQL 中的历史",
					},
				},
				err: tt.dbErr,
			}

			svc := NewMessageService(messages, nil, contexts, nil, fakeChatResponder{})

			history, err := svc.loadHistory(context.Background(), 1)
			if err != nil {
				t.Fatalf("load history failed: %v", err)
			}

			if !contexts.listCalled {
				t.Fatal("Redis history must be checked first")
			}
			if contexts.conversationID != 1 {
				t.Fatal("unexpected conversation ID passed to Redis")
			}
			if messages.listed != tt.wantDBCall {
				t.Fatalf(
					"expected MySQL query %v, got %v",
					tt.wantDBCall,
					messages.listed,
				)
			}
			wantLatestCheck := tt.cacheErr == nil && len(tt.cached) > 0
			if messages.latestChecked != wantLatestCheck {
				t.Fatalf(
					"expected latest ID check %v, got %v",
					wantLatestCheck,
					messages.latestChecked,
				)
			}
			if len(history) != 1 {
				t.Fatalf("expected 1 history message, got %d", len(history))
			}
			if history[0].Role != "user" ||
				history[0].Content != tt.wantContent {
				t.Fatalf("unexpected history: %+v", history)
			}
		})
	}
}

func TestMessageServiceLoadHistoryCanceled(t *testing.T) {
	contexts := &messageContextStub{}
	messages := &fakeMessageStore{}
	svc := NewMessageService(messages, nil, contexts, nil, fakeChatResponder{})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	history, err := svc.loadHistory(ctx, 1)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if history != nil {
		t.Fatal("canceled request must not return history")
	}
	if contexts.listCalled {
		t.Fatal("canceled request must not query Redis")
	}
	if messages.listed {
		t.Fatal("canceled request must not query MySQL")
	}
}

func TestMessageServiceLoadHistoryDatabaseFailure(t *testing.T) {
	dbErr := errors.New("mysql unavailable")

	contexts := &messageContextStub{}
	messages := &fakeMessageStore{err: dbErr}
	svc := NewMessageService(messages, nil, contexts, nil, fakeChatResponder{})

	history, err := svc.loadHistory(context.Background(), 1)

	if !errors.Is(err, dbErr) {
		t.Fatalf("expected database error, got %v", err)
	}
	if history != nil {
		t.Fatal("database failure must not return history")
	}
	if !messages.listed {
		t.Fatal("empty cache must trigger a MySQL query")
	}
}

func TestMessageServiceLoadHistoryRejectsStaleCache(t *testing.T) {
	messages := &fakeMessageStore{
		messages: []model.Message{
			{
				ID:             1,
				ConversationID: 1,
				Role:           "user",
				Content:        "我的投稿被退回了",
			},
			{
				ID:             2,
				ConversationID: 1,
				Role:           "assistant",
				Content:        "页面显示了什么提示？",
			},
			{
				ID:             3,
				ConversationID: 1,
				Role:           "user",
				Content:        "提示标题不符合要求",
			},
			{
				ID:             4,
				ConversationID: 1,
				Role:           "assistant",
				Content:        "请提供标题相关的完整提示。",
			},
		},
	}

	// 模拟上一轮数据库提交成功，但缓存刷新失败。
	contexts := &messageContextStub{
		history: []cache.ContextMessage{
			{
				ID:      1,
				Role:    "user",
				Content: "我的投稿被退回了",
			},
			{
				ID:      2,
				Role:    "assistant",
				Content: "页面显示了什么提示？",
			},
		},
	}

	svc := NewMessageService(
		messages,
		nil,
		contexts,
		nil,
		fakeChatResponder{},
	)

	history, err := svc.loadHistory(context.Background(), 1)
	if err != nil {
		t.Fatalf("load history failed: %v", err)
	}

	if len(history) != 4 {
		t.Fatalf(
			"stale cache must not hide committed messages: want 4, got %d",
			len(history),
		)
	}

	for i, want := range messages.messages {
		if history[i].Role != want.Role ||
			history[i].Content != want.Content {
			t.Fatalf(
				"history[%d] does not match committed database history",
				i,
			)
		}
	}
}

func TestMessageServiceLoadHistoryCacheValidationFailure(t *testing.T) {
	dbErr := errors.New("latest message query failed")

	messages := &fakeMessageStore{err: dbErr}
	contexts := &messageContextStub{
		history: []cache.ContextMessage{
			{
				ID:      1,
				Role:    "user",
				Content: "不能确认是否最新的缓存",
			},
		},
	}

	svc := NewMessageService(
		messages,
		nil,
		contexts,
		nil,
		fakeChatResponder{},
	)

	history, err := svc.loadHistory(context.Background(), 1)

	if !errors.Is(err, dbErr) {
		t.Fatalf("expected database validation error, got %v", err)
	}
	if history != nil {
		t.Fatal("unverified cache must not be returned")
	}
	if !messages.latestChecked {
		t.Fatal("non-empty cache must be validated")
	}
	if messages.listed {
		t.Fatal("validation failure must return without another history query")
	}
}
