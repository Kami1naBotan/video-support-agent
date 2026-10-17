package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

type ContextMessage struct {
	ID      uint64 `json:"id"`
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ContextStore struct {
	client      *redis.Client
	maxMessages int64
	ttl         time.Duration
}

func NewContextStore(client *redis.Client, maxMessages int64, ttl time.Duration) (*ContextStore, error) {
	if client == nil {
		return nil, fmt.Errorf("redis client is required")
	}
	if maxMessages <= 0 {
		return nil, fmt.Errorf("max messages must be positive")
	}
	if ttl < time.Second {
		return nil, fmt.Errorf("context TTL must be at least one second")
	}

	return &ContextStore{
		client:      client,
		maxMessages: maxMessages,
		ttl:         ttl,
	}, nil
}

func contextKey(conversationID uint64) string {
	return fmt.Sprintf("conversation:context:%d", conversationID)
}

func (s *ContextStore) Append(ctx context.Context, conversationID uint64, message ContextMessage) error {
	if conversationID == 0 || message.ID == 0 {
		return fmt.Errorf("conversation ID and message ID must be positive")
	}

	data, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("encode context message: %w", err)
	}

	key := contextKey(conversationID)

	_, err = s.client.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		pipe.RPush(ctx, key, string(data))
		pipe.LTrim(ctx, key, -s.maxMessages, -1)
		pipe.Expire(ctx, key, s.ttl)
		return nil
	})
	if err != nil {
		return fmt.Errorf("append context : %w", err)
	}

	return nil
}

func (s *ContextStore) List(ctx context.Context, conversationID uint64) ([]ContextMessage, error) {
	if conversationID == 0 {
		return nil, fmt.Errorf("conversation ID must be positive")
	}

	values, err := s.client.LRange(ctx, contextKey(conversationID), 0, -1).Result()
	if err != nil {
		return nil, fmt.Errorf("read context : %w", err)
	}

	messages := make([]ContextMessage, 0, len(values))
	for _, value := range values {
		var message ContextMessage
		if err := json.Unmarshal([]byte(value), &message); err != nil {
			return nil, fmt.Errorf("decode context message: %w", err)
		}
		messages = append(messages, message)
	}

	return messages, nil
}
