package llm

import (
	"context"
	"errors"
	"strings"
	"time"

	"go.uber.org/zap"
)

const maxInputTextBytes = 32 * 1024

var (
	ErrInvalidInput  = errors.New("invalid chat input")
	ErrModelTimeout  = errors.New("model request timed out")
	ErrModelFailed   = errors.New("model request failed")
	ErrEmptyReply    = errors.New("model returned an empty reply")
	ErrInputTooLarge = errors.New("model input text exceeds budget")
)

type HistoryMessage struct {
	Role    string
	Content string
}

type ChatClient struct {
	generator Generator
	streamer  Streamer
	timeout   time.Duration
	logger    *zap.Logger
}

func NewChatClient(generator Generator, timeout time.Duration, logger *zap.Logger) (*ChatClient, error) {
	if generator == nil {
		return nil, errors.New("chat generator is required")
	}
	if timeout <= 0 {
		return nil, errors.New("chat timeout must be positive")
	}
	if logger == nil {
		logger = zap.NewNop()
	}

	streamer, _ := generator.(Streamer)

	return &ChatClient{
		generator: generator,
		timeout:   timeout,
		streamer:  streamer,
		logger:    logger,
	}, nil
}

func (c *ChatClient) Reply(ctx context.Context, history []HistoryMessage, question string) (string, error) {
	messages, err := buildMessage(history, question)
	if err != nil {
		return "", err
	}

	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	reply, err := c.generator.Generate(callCtx, messages)

	if errors.Is(callCtx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		c.logger.Warn(
			"model call failed",
			zap.String("error_kind", string(ErrorKindTimeout)),
		)
		return "", ErrModelTimeout
	}

	if errors.Is(callCtx.Err(), context.Canceled) || errors.Is(err, context.Canceled) {
		c.logger.Debug(
			"model call canceled",
			zap.String("error_kind", string(ErrorKindCanceled)),
		)
		return "", context.Canceled
	}

	if err != nil {
		c.logger.Warn(
			"model call failed",
			zap.String("error_kind", string(classifyModelError(err))),
		)
		return "", ErrModelFailed
	}

	if reply == nil {
		return "", ErrEmptyReply
	}

	answer := strings.TrimSpace(reply.Content)
	if answer == "" {
		return "", ErrEmptyReply
	}

	return answer, nil
}
