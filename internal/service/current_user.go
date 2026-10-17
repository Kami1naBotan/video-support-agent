package service

import (
	"context"
	"errors"
	"fmt"
	"video-support-agent/internal/model"

	"gorm.io/gorm"
)

type CurrentUserStore interface {
	FindByID(context.Context, uint64) (*model.User, error)
}

type CurrentUserService struct {
	users CurrentUserStore
}

func NewCurrentUserService(users CurrentUserStore) *CurrentUserService {
	return &CurrentUserService{users: users}
}

func (s *CurrentUserService) Get(ctx context.Context, userID uint64) (*model.User, error) {
	if userID == 0 {
		return nil, ErrUnauthorized
	}

	user, err := s.users.FindByID(ctx, userID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrUnauthorized
		}
		return nil, fmt.Errorf("get current user: %w", err)
	}

	return user, err
}
