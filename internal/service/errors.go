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
)

func normalizeConversationError(err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrConversationNotFound
	}

	return err
}
