package service

import (
	"context"
	"errors"
	"testing"
	"video-support-agent/internal/model"

	"gorm.io/gorm"
)

type fakeMessageStore struct {
	created  *model.Message
	messages []model.Message
	err      error
}

func (f *fakeMessageStore) Create(
	_ context.Context,
	message *model.Message,
) error {
	f.created = message
	message.ID = 10
	return f.err
}

func (f *fakeMessageStore) ListByConversationID(
	_ context.Context,
	_ uint64,
) ([]model.Message, error) {
	return f.messages, f.err
}

func TestMessageServiceCreate(t *testing.T) {
	messages := &fakeMessageStore{}
	conversations := &fakeConversationStore{
		found: &model.Conversation{
			ID: 1,
		},
	}
	service := NewMessageService(messages, conversations)

	message, err := service.Create(
		context.Background(),
		1,
		"  账号被封禁后如何申诉？  ",
	)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if message.ID != 10 {
		t.Fatalf("expected message id 10, got %d", message.ID)
	}

	if message.ConversationID != 1 {
		t.Fatalf("expected conversation id 1, got %d", message.ConversationID)
	}

	if message.Role != "user" {
		t.Fatalf("expected role user, got %q", message.Role)
	}

	if message.Content != "账号被封禁后如何申诉？" {
		t.Fatalf("expected trimmed content, got %q", message.Content)
	}
}

func TestMessageServiceCreateInvalidArgument(t *testing.T) {
	messages := &fakeMessageStore{}
	conversations := &fakeConversationStore{
		found: &model.Conversation{ID: 1},
	}
	service := NewMessageService(messages, conversations)

	_, err := service.Create(context.Background(), 1, "   ")

	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument, got %v", err)
	}

	if messages.created != nil {
		t.Fatal("message should not be created")
	}
}

func TestMessageServiceCreateConversationNotFound(t *testing.T) {
	messages := &fakeMessageStore{}
	conversations := &fakeConversationStore{
		err: gorm.ErrRecordNotFound,
	}
	service := NewMessageService(messages, conversations)

	_, err := service.Create(context.Background(), 999, "测试")

	if !errors.Is(err, ErrConversationNotFound) {
		t.Fatalf("expected ErrConversationNotFound, got %v", err)
	}

	if messages.created != nil {
		t.Fatal("message should not be created")
	}
}

func TestMessageServiceListByConversationID(t *testing.T) {
	messages := &fakeMessageStore{
		messages: []model.Message{
			{
				ID:             1,
				ConversationID: 5,
				Role:           "user",
				Content:        "第一条",
			},
			{
				ID:             2,
				ConversationID: 5,
				Role:           "user",
				Content:        "第二条",
			},
		},
	}
	conversations := &fakeConversationStore{
		found: &model.Conversation{ID: 5},
	}
	service := NewMessageService(messages, conversations)

	result, err := service.ListByConversationID(
		context.Background(),
		5,
	)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if len(result) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(result))
	}

	if result[0].ID != 1 || result[1].ID != 2 {
		t.Fatalf("messages are not returned in expected order: %+v", result)
	}
}

func TestMessageServiceListEmpty(t *testing.T) {
	messages := &fakeMessageStore{
		messages: []model.Message{},
	}
	conversations := &fakeConversationStore{
		found: &model.Conversation{ID: 5},
	}
	service := NewMessageService(messages, conversations)

	result, err := service.ListByConversationID(
		context.Background(),
		5,
	)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if result == nil {
		t.Fatal("expected empty slice, got nil")
	}

	if len(result) != 0 {
		t.Fatalf("expected zero messages, got %d", len(result))
	}
}
