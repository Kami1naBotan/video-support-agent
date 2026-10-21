package service

import (
	"context"
	"errors"
	"time"
	"video-support-agent/internal/llm"

	"go.uber.org/zap"
)

func (s *MessageService) logChatStreamFinished(started time.Time, stage string, saved bool, err error) {
	outcome := "failed"

	switch {
	case err == nil && saved:
		outcome = "success"
	case errors.Is(err, context.Canceled):
		outcome = "canceled"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, llm.ErrModelTimeout):
		outcome = "timeout"
	case errors.Is(err, ErrConversationBusy):
		outcome = "busy"
	}

	fields := []zap.Field{
		zap.String("stage", stage),
		zap.String("outcome", outcome),
		zap.Bool("saved", saved),
		zap.Duration("duration", time.Since(started)),
	}

	if outcome == "success" || outcome == "canceled" || outcome == "busy" {
		s.logger.Info("chat stream finished", fields...)
		return
	}

	s.logger.Warn("chat stream finished", fields...)
}
