package service

import (
	"errors"

	"gorm.io/gorm"
)

var (
	ErrInvalidArgument      = errors.New("invalid argument")
	ErrUserNotFound         = errors.New("user not found")
	ErrConversationNotFound = errors.New("conversation not found")
	ErrMessageNotFound      = errors.New("message not found")
	ErrUsernameExists       = errors.New("username already exists")
	ErrInvalidCredentials   = errors.New("invalid credentials")
	ErrUnauthorized         = errors.New("unauthorized")
	ErrConversationBusy     = errors.New("conversation is busy")
)

func normalizeConversationError(err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrConversationNotFound
	}

	return err
}
