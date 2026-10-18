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

const demoReply = "已收到你的咨询。当前为功能演示，尚未接入规则检索与真实模型，暂不提供具体规则结论。"

type MessageStore interface {
	CreatePair(context.Context, *model.Message, *model.Message) error
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

func (s *MessageService) appendContext(ctx context.Context, message *model.Message) {
	if s.contexts == nil {
		return
	}

	cacheCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()

	err := s.contexts.Append(
		cacheCtx,
		message.ConversationID,
		cache.ContextMessage{
			ID:      message.ID,
			Role:    message.Role,
			Content: message.Content,
		},
	)

	if err != nil {
		s.logger.Warn(
			"update conversation context failed",
			zap.Uint64("conversation_id", message.ConversationID),
			zap.Uint64("message_id", message.ID),
			zap.Error(err),
		)
	}
}

func (s *MessageService) Create(ctx context.Context, userID uint64, conversationID uint64, content string) (*model.Message, error) {
	if err := s.checkOwnership(ctx, userID, conversationID); err != nil {
		return nil, err
	}

	content = strings.TrimSpace(content)
	if conversationID == 0 || content == "" || utf8.RuneCountInString(content) > 10000 {
		return nil, fmt.Errorf("%w: content must contain 1 to 10000 characters", ErrInvalidArgument)
	}

	userMessage := &model.Message{
		ConversationID: conversationID,
		Role:           "user",
		Content:        content,
	}

	assistantMessage := &model.Message{
		ConversationID: conversationID,
		Role:           "assistant",
		Content:        demoReply,
	}

	if err := s.messages.CreatePair(ctx, userMessage, assistantMessage); err != nil {
		return nil, err
	}

	for _, message := range []*model.Message{userMessage, assistantMessage} {
		s.appendContext(ctx, message)
	}

	return userMessage, nil
}

func (s *MessageService) ListByConversationID(ctx context.Context, userID uint64, conversationID uint64) ([]model.Message, error) {
	if err := s.checkOwnership(ctx, userID, conversationID); err != nil {
		return nil, err
	}

	return s.messages.ListByConversationID(ctx, conversationID)
}
