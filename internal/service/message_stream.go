package service

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
	"video-support-agent/internal/llm"
	"video-support-agent/internal/model"
)

type ChatStreamResponder interface {
	ReplyStream(ctx context.Context, history []llm.HistoryMessage, question string, onChunk func(string) error) (string, error)
}

var _ ChatStreamResponder = (*llm.ChatClient)(nil)

type StreamCallbacks struct {
	OnStart func() error
	OnChunk func(string) error
}

type StreamResult struct {
	UserMessage      *model.Message
	AssistantMessage *model.Message
}

const streamSaveTimeout = 3 * time.Second

func (s *MessageService) CreateStream(ctx context.Context, userID uint64, conversationID uint64, content string, callbacks StreamCallbacks) (result *StreamResult, returnErr error) {
	started := time.Now()
	stage := "prepare"
	saved := false

	defer func() {
		s.logChatStreamFinished(started, stage, saved, returnErr)
	}()

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if err := s.checkOwnership(ctx, userID, conversationID); err != nil {
		return nil, err
	}

	content = strings.TrimSpace(content)
	if content == "" || utf8.RuneCountInString(content) > 10000 {
		return nil, fmt.Errorf("%w: content must contain 1 to 10000 characters", ErrInvalidArgument)
	}

	if callbacks.OnStart == nil || callbacks.OnChunk == nil {
		return nil, fmt.Errorf("%w: stream callbacks are required", ErrInvalidArgument)
	}

	if s.streamChat == nil {
		return nil, llm.ErrStreamUnsupported
	}

	release, acquired := s.guard.TryAcquire(conversationID)
	if !acquired {
		return nil, ErrConversationBusy
	}
	defer release()

	stage = "load_history"

	history, err := s.loadHistory(ctx, conversationID)
	if err != nil {
		return nil, err
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	stage = "start"

	if err := callbacks.OnStart(); err != nil {
		return nil, err
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	stage = "generate"

	answer, err := s.streamChat.ReplyStream(ctx, history, content, func(part string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return callbacks.OnChunk(part)
	})
	if err != nil {
		return nil, err
	}

	if strings.TrimSpace(answer) == "" {
		return nil, llm.ErrEmptyReply
	}

	stage = "save"

	saveCtx, cancelSave := context.WithTimeout(context.WithoutCancel(ctx), streamSaveTimeout)
	defer cancelSave()

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

	if err := s.messages.CreatePair(saveCtx, userMessage, assistantMessage); err != nil {
		return nil, err
	}

	saved = true
	stage = "refresh_context"

	s.refreshContext(saveCtx, conversationID)

	stage = "completed"

	return &StreamResult{
		UserMessage:      userMessage,
		AssistantMessage: assistantMessage,
	}, nil
}
