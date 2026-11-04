package service

import (
	"context"
	"crypto/rand"
	"fmt"
	"time"

	"video-support-agent/internal/model"
)

const (
	AppealConfirmationPurpose  = "create_appeal_ticket"
	AppealConfirmationLifetime = 10 * time.Minute
	maxAppealConfirmationBytes = 8192
)

type AppealConfirmationIssuer interface {
	Issue(context.Context, AppealConfirmationBinding, time.Time, time.Time) (string, error)
}

type AppealConfirmations interface {
	AppealConfirmationIssuer
	AppealConfirmationVerifier
}

type AppealConfirmationInput struct {
	IdempotencyKey string `json:"-"`
	PolicyDigest   string `json:"-"`
	Token          string `json:"-"`
}

type PreparedAppeal struct {
	Input         CreateAppealInput               `json:"-"`
	Eligibility   model.AppealEligibilitySnapshot `json:"-"`
	InitialStatus model.AppealTicketStatus        `json:"-"`
	Binding       AppealConfirmationBinding       `json:"-"`
	Token         string                          `json:"-"`
	IssuedAt      time.Time                       `json:"-"`
	ExpiresAt     time.Time                       `json:"-"`
}

func newAppealOperationKey() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate appeal operation key: %w", err)
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x",
		value[0:4], value[4:6], value[6:8], value[8:10], value[10:16]), nil
}

func validAppealConfirmationToken(token string) bool {
	if len(token) == 0 || len(token) > maxAppealConfirmationBytes {
		return false
	}
	for i := range token {
		if token[i] < 0x21 || token[i] > 0x7e {
			return false
		}
	}
	return true
}

func (s *AppealTicketService) verifyConfirmation(ctx context.Context, token string, binding AppealConfirmationBinding) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.confirmations == nil {
		return ErrAppealConfiguration
	}
	if err := ValidateAppealConfirmationBinding(binding); err != nil {
		return err
	}
	if !validAppealConfirmationToken(token) {
		return ErrAppealConfirmationRequired
	}
	if err := s.confirmations.Verify(ctx, token, binding); err != nil {
		return err
	}
	return ctx.Err()
}
