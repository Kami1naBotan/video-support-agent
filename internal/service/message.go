package service

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
	"video-support-agent/internal/cache"
	"video-support-agent/internal/llm"
	"video-support-agent/internal/model"

	"go.uber.org/zap"
)

type MessageStore interface {
	CreatePair(context.Context, *model.Message, *model.Message) error
	ListByConversationID(context.Context, uint64) ([]model.Message, error)
	ListRecentByConversationID(context.Context, uint64, int) ([]model.Message, error)
	LatestIDByConversationID(context.Context, uint64) (uint64, error)
}

type ChatResponder interface {
	Reply(context.Context, []llm.HistoryMessage, string) (string, error)
}

var _ ChatResponder = (*llm.ChatClient)(nil)

type MessageContextStore interface {
	List(context.Context, uint64) ([]cache.ContextMessage, error)
	Replace(context.Context, uint64, []cache.ContextMessage) error
}

type MessageService struct {
	messages      MessageStore
	conversations ConversationStore
	contexts      MessageContextStore
	logger        *zap.Logger
	chat          ChatResponder
	guard         *ConversationGuard
}

func NewMessageService(messages MessageStore, conversations ConversationStore, contexts MessageContextStore, logger *zap.Logger, chat ChatResponder) *MessageService {
	if logger == nil {
		logger = zap.NewNop()
	}

	return &MessageService{
		messages:      messages,
		conversations: conversations,
		contexts:      contexts,
		logger:        logger,
		chat:          chat,
		guard:         NewConversationGuard(),
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

func (s *MessageService) refreshContext(ctx context.Context, conversationID uint64) {
	if s.contexts == nil {
		return
	}

	refreshCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()

	messages, err := s.messages.ListRecentByConversationID(refreshCtx, conversationID, modelHistoryLimit)
	if err != nil {
		s.logger.Warn(
			"load saved messages for context failed",
			zap.Uint64("conversation_id", conversationID),
		)
		return
	}

	snapshot := make([]cache.ContextMessage, 0, len(messages))
	for _, message := range messages {
		snapshot = append(snapshot, cache.ContextMessage{
			ID:      message.ID,
			Role:    message.Role,
			Content: message.Content,
		})
	}

	if err := s.contexts.Replace(refreshCtx, conversationID, snapshot); err != nil {
		s.logger.Warn(
			"update conversation context failed",
			zap.Uint64("conversation_id", conversationID),
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

	if s.chat == nil {
		return nil, llm.ErrModelFailed
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	release, acquired := s.guard.TryAcquire(conversationID)
	if !acquired {
		return nil, ErrConversationBusy
	}
	defer release()

	history, err := s.loadHistory(ctx, conversationID)
	if err != nil {
		return nil, err
	}

	answer, err := s.chat.Reply(ctx, history, content)
	if err != nil {
		return nil, err
	}

	answer = strings.TrimSpace(answer)
	if answer == "" {
		return nil, llm.ErrEmptyReply
	}

	userMessage := &model.Message{
		ConversationID: conversationID,
		Role:           "user",
		Content:        content,
	}

	assistantMessage := &model.Message{
		ConversationID: conversationID,
		Role:           "assistant",
		Content:        answer,
	}

	if err := s.messages.CreatePair(ctx, userMessage, assistantMessage); err != nil {
		return nil, err
	}

	s.refreshContext(ctx, conversationID)

	return userMessage, nil
}

func (s *MessageService) ListByConversationID(ctx context.Context, userID uint64, conversationID uint64) ([]model.Message, error) {
	if err := s.checkOwnership(ctx, userID, conversationID); err != nil {
		return nil, err
	}

	return s.messages.ListByConversationID(ctx, conversationID)
}
