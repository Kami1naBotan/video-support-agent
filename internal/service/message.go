package service

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
	"video-support-agent/internal/cache"
	"video-support-agent/internal/model"

	"go.uber.org/zap"
)

type MessageStore interface {
	Create(context.Context, *model.Message) error
	ListByConversationID(context.Context, uint64) ([]model.Message, error)
}

type MessageContextStore interface {
	Append(context.Context, uint64, cache.ContextMessage) error
}

type MessageService struct {
	messages      MessageStore
	conversations ConversationStore
	contexts      MessageContextStore
	logger        *zap.Logger
}

func NewMessageService(messages MessageStore, conversations ConversationStore, contexts MessageContextStore, logger *zap.Logger) *MessageService {
	if logger == nil {
		logger = zap.NewNop()
	}

	return &MessageService{
		messages:      messages,
		conversations: conversations,
		contexts:      contexts,
		logger:        logger,
	}
}

func (s *MessageService) checkOwnership(ctx context.Context, userID uint64, conversationID uint64) error {
	if userID == 0 {
		return ErrUnauthorized
	}
	if conversationID == 0 {
		return fmt.Errorf("%w: conversation id must be positive", ErrInvalidArgument)
	}

	conversation, err := s.conversations.GetByID(ctx, conversationID)
	if err != nil {
		return normalizeConversationError(err)
	}
	if conversation.UserID != userID {
		return ErrConversationNotFound
	}

	return nil
}

func (s *MessageService) Create(ctx context.Context, userID uint64, conversationID uint64, content string) (*model.Message, error) {
	if err := s.checkOwnership(ctx, userID, conversationID); err != nil {
		return nil, err
	}

	content = strings.TrimSpace(content)
	if conversationID == 0 || content == "" || utf8.RuneCountInString(content) > 10000 {
		return nil, fmt.Errorf("%w: content must contain 1 to 10000 characters", ErrInvalidArgument)
	}

	message := &model.Message{ConversationID: conversationID, Role: "user", Content: content}
	if err := s.messages.Create(ctx, message); err != nil {
		return nil, err
	}

	if s.contexts != nil {
		cacheCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)

		err := s.contexts.Append(
			cacheCtx,
			conversationID,
			cache.ContextMessage{
				ID:      message.ID,
				Role:    message.Role,
				Content: message.Content,
			},
		)
		cancel()

		if err != nil {
			s.logger.Warn(
				"update conversation context failed",
				zap.Uint64("conversation_id", conversationID),
				zap.Uint64("message_id", message.ID),
				zap.Error(err),
			)
		}
	}

	return message, nil
}

func (s *MessageService) ListByConversationID(ctx context.Context, userID uint64, conversationID uint64) ([]model.Message, error) {
	if err := s.checkOwnership(ctx, userID, conversationID); err != nil {
		return nil, err
	}

	return s.messages.ListByConversationID(ctx, conversationID)
}
