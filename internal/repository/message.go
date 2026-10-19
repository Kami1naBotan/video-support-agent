package repository

import (
	"context"
	"fmt"
	"video-support-agent/internal/model"

	"gorm.io/gorm"
)

type MessageRepository struct {
	db *gorm.DB
}

func NewMessageRepository(db *gorm.DB) *MessageRepository {
	return &MessageRepository{db: db}
}

func (r *MessageRepository) Create(ctx context.Context, message *model.Message) error {
	err := r.db.WithContext(ctx).Omit("Conversation").Create(message).Error
	if err != nil {
		return fmt.Errorf("create message: %w", err)
	}

	return nil
}

func (r *MessageRepository) ListByConversationID(ctx context.Context, conversationID uint64) ([]model.Message, error) {
	var messages []model.Message

	err := r.db.WithContext(ctx).Where("conversation_id = ?", conversationID).Order("id ASC").Find(&messages).Error
	if err != nil {
		return nil, fmt.Errorf("list messages by conversation id: %w", err)
	}

	return messages, nil
}

func (r *MessageRepository) CreatePair(ctx context.Context, userMessage *model.Message, assistantMessage *model.Message) error {
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Omit("Conversation").Create(userMessage).Error; err != nil {
			return fmt.Errorf("create user message: %w", err)
		}

		if err := tx.Omit("Conversation").Create(assistantMessage).Error; err != nil {
			return fmt.Errorf("create assistant message: %w", err)
		}

		return nil
	})

	if err != nil {
		return fmt.Errorf("create message pair: %w", err)
	}

	return nil
}

func (r *MessageRepository) ListRecentByConversationID(ctx context.Context, conversationID uint64, limit int) ([]model.Message, error) {
	if conversationID == 0 {
		return nil, fmt.Errorf("conversation ID must be positive")
	}
	if limit <= 0 || limit > 100 {
		return nil, fmt.Errorf("message limit must be between 1 and 100")
	}

	messages := make([]model.Message, 0)

	err := r.db.WithContext(ctx).Where("conversation_id = ?", conversationID).Order("id DESC").Limit(limit).Find(&messages).Error
	if err != nil {
		return nil, fmt.Errorf("list recent messages: %w", err)
	}

	for left, right := 0, len(messages)-1; left < right; left, right = left+1, right-1 {
		messages[left], messages[right] = messages[right], messages[left]
	}

	return messages, nil
}

func (r *MessageRepository) LatestIDByConversationID(ctx context.Context, conversationID uint64) (uint64, error) {
	if conversationID == 0 {
		return 0, fmt.Errorf("conversation ID must be positive")
	}

	var ids []uint64

	err := r.db.WithContext(ctx).Model(&model.Message{}).Where("conversation_id = ?", conversationID).Order("id DESC").Limit(1).Pluck("id", &ids).Error
	if err != nil {
		return 0, fmt.Errorf("query latest message IDs: %w", err)
	}

	if len(ids) == 0 {
		return 0, nil
	}

	return ids[0], nil
}
