package llm

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"

	"go.uber.org/zap"
)

var (
	ErrStreamUnsupported = errors.New("model does not support streaming")
	ErrIncompleteReply   = errors.New("model reply did not finish normally")
)

func (c *ChatClient) streamFailure(ctx context.Context, err error) error {
	if errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled) {
		return context.Canceled
	}

	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return ErrModelTimeout
	}

	c.logger.Warn(
		"model stream failed",
		zap.String("error_kind", string(classifyModelError(err))),
	)

	return ErrModelFailed
}

func (c *ChatClient) ReplyStream(ctx context.Context, history []HistoryMessage, question string, onChunk func(string) error) (answerText string, returnErr error) {
	started := time.Now()

	defer func() {
		c.logStreamFinished(started, returnErr)
	}()

	if onChunk == nil {
		return "", ErrInvalidInput
	}

	messages, err := buildMessage(history, question)
	if err != nil {
		return "", err
	}

	if c.streamer == nil {
		return "", ErrStreamUnsupported
	}

	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	if err := callCtx.Err(); err != nil {
		return "", c.streamFailure(callCtx, err)
	}

	reader, err := c.streamer.Stream(callCtx, messages)
	if err != nil {
		return "", c.streamFailure(callCtx, err)
	}
	if reader == nil {
		return "", ErrModelFailed
	}

	defer reader.Close()

	var answer strings.Builder
	var finishReason string

	for {
		if err := callCtx.Err(); err != nil {
			return "", c.streamFailure(callCtx, err)
		}

		chunk, recvErr := reader.Recv()

		if err := callCtx.Err(); err != nil {
			return "", c.streamFailure(callCtx, err)
		}

		if errors.Is(recvErr, io.EOF) {
			break
		}
		if recvErr != nil {
			return "", c.streamFailure(callCtx, recvErr)
		}
		if chunk == nil {
			continue
		}

		if chunk.ResponseMeta != nil && chunk.ResponseMeta.FinishReason != "" {
			finishReason = chunk.ResponseMeta.FinishReason
		}

		if chunk.Content == "" {
			continue
		}

		answer.WriteString(chunk.Content)

		if err := onChunk(chunk.Content); err != nil {
			return "", err
		}
	}

	if finishReason != "stop" {
		return "", ErrIncompleteReply
	}

	fullAnswer := answer.String()
	if strings.TrimSpace(fullAnswer) == "" {
		return "", ErrEmptyReply
	}

	return fullAnswer, nil
}
