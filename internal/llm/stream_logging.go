package llm

import (
	"context"
	"errors"
	"time"

	"go.uber.org/zap"
)

func streamOutcome(err error) string {
	switch {
	case err == nil:
		return "success"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, ErrModelTimeout), errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, ErrInvalidInput):
		return "invalid_input"
	case errors.Is(err, ErrInputTooLarge):
		return "input_too_large"
	case errors.Is(err, ErrStreamUnsupported):
		return "unsupported"
	case errors.Is(err, ErrIncompleteReply):
		return "incomplete"
	case errors.Is(err, ErrEmptyReply):
		return "empty_reply"
	case errors.Is(err, ErrModelFailed):
		return "model_failed"
	default:
		return "failed"
	}
}

func (c *ChatClient) logStreamFinished(started time.Time, err error) {
	outcome := streamOutcome(err)

	fields := []zap.Field{
		zap.String("outcome", outcome),
		zap.Duration("duration", time.Since(started)),
	}

	if outcome == "success" || outcome == "canceled" {
		c.logger.Info("model stream finished", fields...)
		return
	}

	c.logger.Warn("model stream finished", fields...)
}
