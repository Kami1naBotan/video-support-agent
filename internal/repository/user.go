package repository

import (
	"context"
	"fmt"
	"video-support-agent/internal/model"

	"gorm.io/gorm"
)

type UserRepository struct {
	db *gorm.DB
}

func NewUserRepository(db *gorm.DB) *UserRepository {
	return &UserRepository{db: db}
}

func (r *UserRepository) Exists(ctx context.Context, id uint64) (bool, error) {
	var count int64

	err := r.db.WithContext(ctx).Model(&model.User{}).Where("id = ?", id).Count(&count).Error
	if err != nil {
		return false, fmt.Errorf("check user existence: %w", err)
	}

	return count > 0, nil
}
