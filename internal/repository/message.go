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
