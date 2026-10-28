package service

import (
	"context"
	"errors"
	"testing"
	"video-support-agent/internal/cache"
	"video-support-agent/internal/llm"
	"video-support-agent/internal/model"
)

type recordingChatResponder struct {
	calls    int
	history  []llm.HistoryMessage
	question string
	answer   string
	err      error
}

func (f *recordingChatResponder) Reply(
	_ context.Context,
	history []llm.HistoryMessage,
	question string,
) (string, error) {
	f.calls++
	f.history = append([]llm.HistoryMessage(nil), history...)
	f.question = question

	return f.answer, f.err
}

func TestMessageServiceChatSuccess(t *testing.T) {
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
				Content:        "页面提示了什么？",
			},
		},
	}
	conversations := &fakeConversationStore{
		found: &model.Conversation{ID: 1, UserID: 7},
	}
	contexts := &messageContextStub{
		history: []cache.ContextMessage{
			{ID: 1, Role: "user", Content: "我的投稿被退回了"},
			{ID: 2, Role: "assistant", Content: "页面提示了什么？"},
		},
	}
	chat := &recordingChatResponder{
		answer: "  请提供更完整的页面提示。  ",
	}

	svc := NewMessageService(
		messages, conversations, contexts, nil, chat,
	)

	got, err := svc.Create(
		context.Background(), 7, 1, "  提示内容不符合要求  ",
	)
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	if got == nil || got.ID != 10 {
		t.Fatal("expected the saved user message")
	}

	if chat.calls != 1 {
		t.Fatalf("expected 1 model call, got %d", chat.calls)
	}
	if chat.question != "提示内容不符合要求" {
		t.Fatalf("unexpected question: %q", chat.question)
	}
	if len(chat.history) != 2 {
		t.Fatalf("expected 2 historical messages, got %d", len(chat.history))
	}
	if chat.history[0].Role != "user" ||
		chat.history[0].Content != "我的投稿被退回了" ||
		chat.history[1].Role != "assistant" ||
		chat.history[1].Content != "页面提示了什么？" {
		t.Fatalf("unexpected model history: %+v", chat.history)
	}

	if !messages.listed {
		t.Fatal("successful chat must reload saved history for the cache")
	}
	if messages.created == nil || messages.reply == nil {
		t.Fatal("both user and assistant messages must be saved")
	}
	if messages.created.Content != chat.question {
		t.Fatal("saved user message must match the current question")
	}
	if messages.reply.Content != "请提供更完整的页面提示。" {
		t.Fatalf("unexpected saved reply: %q", messages.reply.Content)
	}

	if len(contexts.received) != 4 {
		t.Fatal("cache snapshot must include old history and the new message pair")
	}

	if contexts.received[0].ID != 1 ||
		contexts.received[1].ID != 2 {
		t.Fatal("cache snapshot must preserve old history")
	}

	if contexts.received[2].ID != messages.created.ID ||
		contexts.received[3].ID != messages.reply.ID {
		t.Fatal("cache snapshot must include the newly saved message pair")
	}
}

func TestMessageServiceChatFailureDoesNotSave(t *testing.T) {
	tests := []struct {
		name    string
		answer  string
		err     error
		wantErr error
	}{
		{
			name:    "model unavailable",
			err:     llm.ErrModelFailed,
			wantErr: llm.ErrModelFailed,
		},
		{
			name:    "model timeout",
			err:     llm.ErrModelTimeout,
			wantErr: llm.ErrModelTimeout,
		},
		{
			name:    "request canceled",
			err:     context.Canceled,
			wantErr: context.Canceled,
		},
		{
			name:    "empty answer",
			answer:  " \n ",
			wantErr: llm.ErrEmptyReply,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
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
						Content:        "页面提示了什么？",
					},
				},
			}
			conversations := &fakeConversationStore{
				found: &model.Conversation{ID: 1, UserID: 7},
			}
			contexts := &messageContextStub{}
			chat := &recordingChatResponder{
				answer: tt.answer,
				err:    tt.err,
			}

			svc := NewMessageService(
				messages, conversations, contexts, nil, chat,
			)

			got, err := svc.Create(
				context.Background(), 7, 1, "我的投稿被退回了",
			)

			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("expected %v, got %v", tt.wantErr, err)
			}
			if chat.calls != 1 {
				t.Fatalf("expected 1 model call, got %d", chat.calls)
			}
			if got != nil || messages.created != nil || messages.reply != nil {
				t.Fatal("failed chat must not save or return messages")
			}
			if contexts.called {
				t.Fatal("failed chat must not append messages to Redis")
			}
		})
	}
}

func TestMessageServiceChatRejectsOtherUser(t *testing.T) {
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
				Content:        "页面提示了什么？",
			},
		},
	}
	conversations := &fakeConversationStore{
		found: &model.Conversation{ID: 1, UserID: 8},
	}
	contexts := &messageContextStub{}
	chat := &recordingChatResponder{answer: "不应生成的回答"}

	svc := NewMessageService(
		messages, conversations, contexts, nil, chat,
	)

	got, err := svc.Create(
		context.Background(), 7, 1, "查询这个会话",
	)

	if !errors.Is(err, ErrConversationNotFound) {
		t.Fatalf("expected ErrConversationNotFound, got %v", err)
	}
	if got != nil {
		t.Fatal("unauthorized request must not return a message")
	}
	if chat.calls != 0 {
		t.Fatal("unauthorized request must not call the model")
	}
	if contexts.listCalled || messages.listed {
		t.Fatal("unauthorized request must not read history")
	}
	if contexts.called || messages.created != nil || messages.reply != nil {
		t.Fatal("unauthorized request must not write messages")
	}
}

func TestMessageServiceChatRestoresEmptyCache(t *testing.T) {
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
				Content:        "页面提示了什么？",
			},
		},
	}
	conversations := &fakeConversationStore{
		found: &model.Conversation{ID: 1, UserID: 7},
	}
	contexts := &messageContextStub{}
	chat := &recordingChatResponder{
		answer: "请提供完整提示。",
	}

	svc := NewMessageService(
		messages, conversations, contexts, nil, chat,
	)

	_, err := svc.Create(
		context.Background(), 7, 1, "提示内容不符合要求",
	)
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	if !contexts.listCalled || !messages.listed {
		t.Fatal("empty cache must fall back to MySQL")
	}
	if chat.calls != 1 || len(chat.history) != 2 {
		t.Fatal("model must receive the existing MySQL history")
	}
	if chat.history[0].Content != "我的投稿被退回了" ||
		chat.history[1].Content != "页面提示了什么？" {
		t.Fatalf("unexpected model history: %+v", chat.history)
	}

	if !contexts.called {
		t.Fatal("successful chat must replace the cache")
	}
	if len(contexts.history) != 4 {
		t.Fatalf(
			"expected old history plus new pair, got %d messages",
			len(contexts.history),
		)
	}

	wantIDs := []uint64{1, 2, 10, 11}
	for i, wantID := range wantIDs {
		if contexts.history[i].ID != wantID {
			t.Fatalf(
				"message %d: expected ID %d, got %d",
				i, wantID, contexts.history[i].ID,
			)
		}
	}

	// 模拟下一轮读取历史，确认使用完整缓存，不再回退。
	messages.listed = false

	history, err := svc.loadHistory(context.Background(), 1)
	if err != nil {
		t.Fatalf("load restored history: %v", err)
	}
	if messages.listed {
		t.Fatal("restored cache should satisfy the next history read")
	}
	if len(history) != 4 {
		t.Fatalf("expected 4 historical messages, got %d", len(history))
	}
	if history[0].Content != "我的投稿被退回了" ||
		history[3].Content != "请提供完整提示。" {
		t.Fatalf("restored history is incomplete: %+v", history)
	}
}

func TestMessageServiceChatSaveFailure(t *testing.T) {
	saveErr := errors.New("database write failed")

	messages := &fakeMessageStore{saveErr: saveErr}
	conversations := &fakeConversationStore{
		found: &model.Conversation{ID: 1, UserID: 7},
	}
	contexts := &messageContextStub{}
	chat := &recordingChatResponder{
		answer: "模型已经成功生成的回答",
	}

	svc := NewMessageService(
		messages, conversations, contexts, nil, chat,
	)

	got, err := svc.Create(
		context.Background(), 7, 1, "我的投稿被退回了",
	)

	if !errors.Is(err, saveErr) {
		t.Fatalf("expected save error, got %v", err)
	}
	if chat.calls != 1 {
		t.Fatal("model must have completed before the save attempt")
	}
	if messages.saveCalls != 1 {
		t.Fatalf("expected 1 save attempt, got %d", messages.saveCalls)
	}
	if got != nil || messages.created != nil || messages.reply != nil {
		t.Fatal("failed save must not return or retain a saved message pair")
	}
	if len(messages.messages) != 0 {
		t.Fatal("failed save must not change stored history")
	}
	if contexts.called {
		t.Fatal("failed save must not update Redis")
	}
}
