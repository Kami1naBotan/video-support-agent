package llm

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/cloudwego/eino/schema"
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

	return &ChatClient{
		generator: generator,
		timeout:   timeout,
		logger:    logger,
	}, nil
}

func (c *ChatClient) Reply(ctx context.Context, history []HistoryMessage, question string) (string, error) {
	question = strings.TrimSpace(question)
	if question == "" {
		return "", ErrInvalidInput
	}

	messages := make([]*schema.Message, 0, len(history)+2)
	messages = append(messages, schema.SystemMessage(supportSystemPrompt))

	for _, item := range history {
		content := strings.TrimSpace(item.Content)
		if content == "" {
			return "", ErrInvalidInput
		}

		switch item.Role {
		case "user":
			messages = append(messages, schema.UserMessage(content))
		case "assistant":
			messages = append(messages, &schema.Message{
				Role:    schema.Assistant,
				Content: content,
			})
		default:
			return "", ErrInvalidInput
		}
	}

	messages = append(messages, schema.UserMessage(question))

	last := len(messages) - 1
	requiredBytes := len(messages[0].Content) + len(messages[last].Content)
	if requiredBytes > maxInputTextBytes {
		return "", ErrInputTooLarge
	}

	totalBytes := requiredBytes
	for _, message := range messages[1:last] {
		totalBytes += len(message.Content)
	}

	start := 1
	for totalBytes > maxInputTextBytes && start < last {
		totalBytes -= len(messages[start].Content)
		start++

		for start < last && messages[start].Role != schema.User {
			totalBytes -= len(messages[start].Content)
			start++
		}
	}

	if start > 1 {
		trimmed := make([]*schema.Message, 0, len(messages)-start+1)
		trimmed = append(trimmed, messages[0])
		trimmed = append(trimmed, messages[start:last]...)
		trimmed = append(trimmed, messages[last])
		messages = trimmed
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
