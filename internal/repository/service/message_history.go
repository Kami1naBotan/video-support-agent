package service

import (
	"context"
	"fmt"
	"time"
	"video-support-agent/internal/llm"

	"go.uber.org/zap"
)

const modelHistoryLimit = 20

func (s *MessageService) loadHistory(ctx context.Context, conversationID uint64) ([]llm.HistoryMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if s.contexts != nil {
		cacheCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		cached, err := s.contexts.List(cacheCtx, conversationID)
		cancel()

		if parentErr := ctx.Err(); parentErr != nil {
			return nil, parentErr
		}

		if err == nil && len(cached) > 0 {
			latestID, checkErr := s.messages.LatestIDByConversationID(ctx, conversationID)
			if checkErr != nil {
				return nil, fmt.Errorf("validate conversation cache: %w", checkErr)
			}

			if parentErr := ctx.Err(); parentErr != nil {
				return nil, parentErr
			}

			cachedLatestID := cached[len(cached)-1].ID

			if latestID != 0 && cachedLatestID == latestID {
				if len(cached) > modelHistoryLimit {
					cached = cached[len(cached)-modelHistoryLimit:]
				}

				history := make([]llm.HistoryMessage, 0, len(cached))
				for _, message := range cached {
					history = append(history, llm.HistoryMessage{
						Role:    message.Role,
						Content: message.Content,
					})
				}

				return history, nil
			}
		}

		if err != nil {
			s.logger.Warn(
				"read conversation context failed; falling back ti MySQL",
				zap.Uint64("conversation_id", conversationID),
			)
		}
	}

	stored, err := s.messages.ListRecentByConversationID(ctx, conversationID, modelHistoryLimit)
	if err != nil {
		return nil, fmt.Errorf("load conversation history: %w", err)
	}

	history := make([]llm.HistoryMessage, 0, len(stored))
	for _, message := range stored {
		history = append(history, llm.HistoryMessage{
			Role:    message.Role,
			Content: message.Content,
		})
	}

	return history, nil
}
