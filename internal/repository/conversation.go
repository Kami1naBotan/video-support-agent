package repository

import (
	"context"
	"fmt"
	"video-support-agent/internal/model"

	"gorm.io/gorm"
)

type ConversationRepository struct {
	db *gorm.DB
}

func NewConversationRepository(db *gorm.DB) *ConversationRepository {
	return &ConversationRepository{db: db}
}

func (r *ConversationRepository) Create(ctx context.Context, conversation *model.Conversation) error {
	err := r.db.WithContext(ctx).Omit("User").Create(conversation).Error
	if err != nil {
		return fmt.Errorf("create conversation: %w", err)
	}

	return nil
}

func (r *ConversationRepository) GetByID(ctx context.Context, id uint64) (*model.Conversation, error) {
	var conversation model.Conversation

	err := r.db.WithContext(ctx).First(&conversation, id).Error
	if err != nil {
		return nil, fmt.Errorf("get conversation by id: %w", err)
	}

	return &conversation, nil
}

func (r *ConversationRepository) ListByUserID(ctx context.Context, userID uint64) ([]model.Conversation, error) {
	conversations := make([]model.Conversation, 0)

	err := r.db.WithContext(ctx).Where("user_id = ?", userID).Order("id DESC").Find(&conversations).Error
	if err != nil {
		return nil, fmt.Errorf("list conversations: %w", err)
	}

	return conversations, nil
}
