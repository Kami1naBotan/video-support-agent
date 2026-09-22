package service

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"
	"video-support-agent/internal/model"
)

type ConversationStore interface {
	Create(context.Context, *model.Conversation) error
	GetByID(context.Context, uint64) (*model.Conversation, error)
}

type UserStore interface {
	Exists(context.Context, uint64) (bool, error)
}

type ConversationService struct {
	conversations ConversationStore
	users         UserStore
}

func NewConversationService(conversations ConversationStore, users UserStore) *ConversationService {
	return &ConversationService{
		conversations: conversations,
		users:         users,
	}
}

func (s *ConversationService) Create(ctx context.Context, userID uint64, title string) (*model.Conversation, error) {
	title = strings.TrimSpace(title)

	if userID == 0 || title == "" || utf8.RuneCountInString(title) > 200 {
		return nil, fmt.Errorf("%w: title must contain 1 to 200 characters", ErrInvalidArgument)
	}

	exists, err := s.users.Exists(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("check user: %w", err)
	}
	if !exists {
		return nil, ErrUserNotFound
	}

	conversation := &model.Conversation{
		UserID: userID,
		Title:  title,
	}

	if err := s.conversations.Create(ctx, conversation); err != nil {
		return nil, err
	}

	return conversation, nil

}

func (s *ConversationService) GetByID(ctx context.Context, id uint64) (*model.Conversation, error) {
	if id == 0 {
		return nil, fmt.Errorf("%w: conversation id must be positive", ErrInvalidArgument)
	}

	conversation, err := s.conversations.GetByID(ctx, id)
	if err != nil {
		return nil, normalizeConversationError(err)
	}

	return conversation, nil
}
