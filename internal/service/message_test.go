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
	listed   bool
	reply    *model.Message
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
	f.listed = true
	return f.messages, f.err
}

func TestMessageServiceCreate(t *testing.T) {
	messages := &fakeMessageStore{}
	conversations := &fakeConversationStore{
		found: &model.Conversation{
			ID:     1,
			UserID: 7,
		},
	}
	service := NewMessageService(messages, conversations, nil, nil)

	message, err := service.Create(
		context.Background(),
		7,
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

	if messages.reply == nil {
		t.Fatal("expected an assistant reply")
	}

	if messages.reply.Role != "assistant" ||
		messages.reply.ConversationID != message.ConversationID ||
		messages.reply.Content != demoReply {
		t.Fatalf("unexpected assistant reply: %+v", messages.reply)
	}
}

func TestMessageServiceCreateInvalidArgument(t *testing.T) {
	messages := &fakeMessageStore{}
	conversations := &fakeConversationStore{
		found: &model.Conversation{ID: 1, UserID: 7},
	}
	service := NewMessageService(messages, conversations, nil, nil)

	_, err := service.Create(context.Background(), 7, 1, "   ")

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
	service := NewMessageService(messages, conversations, nil, nil)

	_, err := service.Create(context.Background(), 7, 999, "测试")

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
		found: &model.Conversation{ID: 5, UserID: 7},
	}
	service := NewMessageService(messages, conversations, nil, nil)

	result, err := service.ListByConversationID(
		context.Background(),
		7,
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
		found: &model.Conversation{ID: 5, UserID: 7},
	}
	service := NewMessageService(messages, conversations, nil, nil)

	result, err := service.ListByConversationID(
		context.Background(),
		7,
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

func TestMessageServiceRejectsUnauthorizedAccess(t *testing.T) {
	dbErr := errors.New("database unavailable")

	tests := []struct {
		name    string
		userID  uint64
		ownerID uint64
		findErr error
		wantErr error
	}{
		{
			name:    "missing identity",
			userID:  0,
			ownerID: 7,
			wantErr: ErrUnauthorized,
		},
		{
			name:    "another user's conversation",
			userID:  7,
			ownerID: 8,
			wantErr: ErrConversationNotFound,
		},
		{
			name:    "missing conversation",
			userID:  7,
			findErr: gorm.ErrRecordNotFound,
			wantErr: ErrConversationNotFound,
		},
		{
			name:    "database failure",
			userID:  7,
			findErr: dbErr,
			wantErr: dbErr,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			messages := &fakeMessageStore{}
			conversations := &fakeConversationStore{
				found: &model.Conversation{
					ID:     1,
					UserID: tt.ownerID,
				},
				err: tt.findErr,
			}
			svc := NewMessageService(messages, conversations, nil, nil)

			message, err := svc.Create(
				context.Background(), tt.userID, 1, "测试",
			)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("create: expected %v, got %v", tt.wantErr, err)
			}
			if message != nil || messages.created != nil {
				t.Fatal("rejected request must not create a message")
			}

			history, err := svc.ListByConversationID(
				context.Background(), tt.userID, 1,
			)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("list: expected %v, got %v", tt.wantErr, err)
			}
			if history != nil || messages.listed {
				t.Fatal("rejected request must not read message history")
			}
		})
	}
}

func (f *fakeMessageStore) CreatePair(
	_ context.Context,
	userMessage *model.Message,
	assistantMessage *model.Message,
) error {
	if f.err != nil {
		return f.err
	}

	userMessage.ID = 10
	assistantMessage.ID = 11
	f.created = userMessage
	f.reply = assistantMessage

	return nil
}
