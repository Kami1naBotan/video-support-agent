package service

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"
	"video-support-agent/internal/model"
)

type MessageStore interface {
	Create(context.Context, *model.Message) error
	ListByConversationID(context.Context, uint64) ([]model.Message, error)
}

type MessageService struct {
	messages      MessageStore
	conversations ConversationStore
}

func NewMessageService(messages MessageStore, conversation ConversationStore) *MessageService {
	return &MessageService{
		messages:      messages,
		conversations: conversation,
	}
}

func (s *MessageService) Create(ctx context.Context, conversationID uint64, content string) (*model.Message, error) {
	content = strings.TrimSpace(content)

	if conversationID == 0 || content == "" || utf8.RuneCountInString(content) > 10000 {
		return nil, fmt.Errorf("%w: content must contain 1 to 10000 characters", ErrInvalidArgument)
	}

	_, err := s.conversations.GetByID(ctx, conversationID)
	if err != nil {
		return nil, normalizeConversationError(err)
	}

	message := &model.Message{
		ConversationID: conversationID,
		Role:           "user",
		Content:        content,
	}

	if err := s.messages.Create(ctx, message); err != nil {
		return nil, err
	}

	return message, nil
}

func (s *MessageService) ListByConversationID(ctx context.Context, conversationID uint64) ([]model.Message, error) {
	if conversationID == 0 {
		return nil, fmt.Errorf("%w: conversation id must be positive", ErrInvalidArgument)
	}

	_, err := s.conversations.GetByID(ctx, conversationID)
	if err != nil {
		return nil, normalizeConversationError(err)
	}

	return s.messages.ListByConversationID(ctx, conversationID)
}
