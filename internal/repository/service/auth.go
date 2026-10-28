package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
	"video-support-agent/internal/model"

	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

type AuthUserStore interface {
	Create(context.Context, *model.User) error
	FindByUsername(context.Context, string) (*model.User, error)
}

type AuthService struct {
	users AuthUserStore
}

func NewAuthService(users AuthUserStore) *AuthService {
	return &AuthService{
		users: users,
	}
}

func (s *AuthService) Register(ctx context.Context, username string, password string) (*model.User, error) {
	username = strings.TrimSpace(username)

	usernameLength := utf8.RuneCountInString(username)
	if usernameLength < 3 || usernameLength > 64 {
		return nil, fmt.Errorf("%w: username must contain 3 to 64 characters", ErrInvalidArgument)
	}

	if len(password) < 8 || len(password) > 72 {
		return nil, fmt.Errorf("%w: password must contain 8 to 72 bytes", ErrInvalidArgument)
	}

	_, err := s.users.FindByUsername(ctx, username)
	switch {
	case err == nil:
		return nil, ErrUsernameExists
	case errors.Is(err, gorm.ErrRecordNotFound):
		//用户名尚未被使用，可以继续注册
	default:
		return nil, fmt.Errorf("find user: %w", err)
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, fmt.Errorf("hash password: %w", err)
	}

	user := &model.User{
		Username:     username,
		PasswordHash: string(hash),
		Role:         model.RoleUser,
	}

	if err := s.users.Create(ctx, user); err != nil {
		return nil, fmt.Errorf("create user: %w", err)
	}

	return user, nil
}

func (s *AuthService) Authenticate(ctx context.Context, username string, password string) (*model.User, error) {
	username = strings.TrimSpace(username)

	if username == "" || len(password) < 8 || len(password) > 72 {
		return nil, ErrInvalidCredentials
	}

	user, err := s.users.FindByUsername(ctx, username)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrInvalidCredentials
		}
		return nil, fmt.Errorf("find user for login: %w", err)
	}

	err = bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password))
	if err != nil {
		return nil, ErrInvalidCredentials
	}

	return user, nil
}
