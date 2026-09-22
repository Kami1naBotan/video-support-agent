package service

import (
	"context"
	"errors"
	"testing"
	"video-support-agent/internal/model"
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

func TestConversationServiceGetByIDNotFound(t *testing.T) {
	users := &fakeUserStore{}
	conversations := &fakeConversationStore{
		err: errors.New("record not found"),
	}
	service := NewConversationService(conversations, users)

	_, err := service.GetByID(context.Background(), 1)

	if err == nil {
		t.Fatal("expected error")
	}
}
