package service

import (
	"context"
	"errors"
	"testing"
	"video-support-agent/internal/model"

	"gorm.io/gorm"
)

type fakeUserStore struct {
	exists bool
	err    error
}

func (f *fakeUserStore) Exists(
	context.Context,
	uint64,
) (bool, error) {
	return f.exists, f.err
}

type fakeConversationStore struct {
	created *model.Conversation
	found   *model.Conversation
	err     error
}

func (f *fakeConversationStore) Create(
	_ context.Context,
	conversation *model.Conversation,
) error {
	f.created = conversation
	conversation.ID = 1
	return nil
}

func (f *fakeConversationStore) GetByID(
	context.Context,
	uint64,
) (*model.Conversation, error) {
	return f.found, f.err
}

func TestConversationServiceCreate(t *testing.T) {
	users := &fakeUserStore{exists: true}
	conversations := &fakeConversationStore{}
	service := NewConversationService(conversations, users)

	conversation, err := service.Create(
		context.Background(),
		7,
		"  账号申诉咨询  ",
	)

	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if conversation.ID != 1 {
		t.Fatalf("expected generated id 1, got %d", conversation.ID)
	}

	if conversation.UserID != 7 {
		t.Fatalf("expected user id 7, got %d", conversation.UserID)
	}

	if conversation.Title != "账号申诉咨询" {
		t.Fatalf("expected trimmed title, got %q", conversation.Title)
	}
}

func TestConversationServiceCreateInvalidArgument(t *testing.T) {
	users := &fakeUserStore{exists: true}
	conversations := &fakeConversationStore{}
	service := NewConversationService(conversations, users)

	_, err := service.Create(context.Background(), 0, "测试")

	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument, got %v", err)
	}

	if conversations.created != nil {
		t.Fatal("conversation should not be created")
	}
}

func TestConversationServiceCreateUserNotFound(t *testing.T) {
	users := &fakeUserStore{exists: false}
	conversations := &fakeConversationStore{}
	service := NewConversationService(conversations, users)

	_, err := service.Create(context.Background(), 999, "测试")

	if !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("expected ErrUserNotFound, got %v", err)
	}

	if conversations.created != nil {
		t.Fatal("conversation should not be created")
	}
}

func TestConversationServiceGetByIDOwnership(t *testing.T) {
	dbErr := errors.New("database unavailable")

	tests := []struct {
		name    string
		userID  uint64
		id      uint64
		found   *model.Conversation
		findErr error
		wantErr error
	}{
		{
			name:   "own conversation",
			userID: 7,
			id:     1,
			found:  &model.Conversation{ID: 1, UserID: 7},
		},
		{
			name:    "another user's conversation",
			userID:  7,
			id:      1,
			found:   &model.Conversation{ID: 1, UserID: 8},
			wantErr: ErrConversationNotFound,
		},
		{
			name:    "missing conversation",
			userID:  7,
			id:      1,
			findErr: gorm.ErrRecordNotFound,
			wantErr: ErrConversationNotFound,
		},
		{
			name:    "missing identity",
			userID:  0,
			id:      1,
			wantErr: ErrUnauthorized,
		},
		{
			name:    "invalid conversation ID",
			userID:  7,
			id:      0,
			wantErr: ErrInvalidArgument,
		},
		{
			name:    "database failure",
			userID:  7,
			id:      1,
			findErr: dbErr,
			wantErr: dbErr,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &fakeConversationStore{
				found: tt.found,
				err:   tt.findErr,
			}
			svc := NewConversationService(store, &fakeUserStore{})

			got, err := svc.GetByID(
				context.Background(),
				tt.userID,
				tt.id,
			)

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("expected %v, got %v", tt.wantErr, err)
				}
				if got != nil {
					t.Fatal("failed query must not return conversation data")
				}
				return
			}

			if err != nil {
				t.Fatal(err)
			}
			if got == nil || got.ID != tt.id || got.UserID != tt.userID {
				t.Fatal("expected the current user's conversation")
			}
		})
	}
}
