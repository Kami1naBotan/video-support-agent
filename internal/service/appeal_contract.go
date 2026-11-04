package service

import (
	"context"
	"time"

	"video-support-agent/internal/model"
)

type AppealEvidenceChecker interface {
	CheckAppealEvidence(context.Context, []model.AppealRuleEvidence, time.Time) (bool, error)
}

type AppealCreateCommand struct {
	UserID               uint64
	ConversationID       uint64
	IdempotencyKey       string
	RequestHash          string
	ExpectedPolicyDigest string
	Input                CreateAppealInput
}

type AppealCreateResult struct {
	TicketID      uint64                   `json:"ticket_id,string"`
	InitialStatus model.AppealTicketStatus `json:"initial_status"`
	CreatedAt     time.Time                `json:"created_at"`
	Replayed      bool                     `json:"replayed"`
}

type AppealTicketView struct {
	TicketID    uint64                   `json:"ticket_id,string"`
	SubjectType model.AppealSubjectType  `json:"subject_type"`
	SubjectRef  string                   `json:"subject_ref"`
	Status      model.AppealTicketStatus `json:"status"`
	CreatedAt   time.Time                `json:"created_at"`
	UpdatedAt   time.Time                `json:"updated_at"`
}

type AppealTransactionCheck func(context.Context, AppealEvidenceChecker, time.Time) (model.AppealEligibilitySnapshot, error)

type AppealTicketStore interface {
	AppealEvidenceChecker
	CheckConversationOwnership(context.Context, uint64, uint64) error
	GetByOperation(context.Context, uint64, string) (*model.AppealTicket, error)
	GetByID(context.Context, uint64, uint64) (*model.AppealTicket, error)
	CreateWithRecord(context.Context, AppealCreateCommand, AppealTransactionCheck) (*AppealCreateResult, error)
}

type AppealConfirmationBinding struct {
	UserID         uint64
	ConversationID uint64
	IdempotencyKey string
	RequestHash    string
	PolicyDigest   string
}

type AppealConfirmationVerifier interface {
	Verify(context.Context, string, AppealConfirmationBinding) error
}

func ValidateAppealConfirmationBinding(binding AppealConfirmationBinding) error {
	if binding.UserID == 0 {
		return ErrUnauthorized
	}
	if binding.ConversationID == 0 {
		return ErrInvalidArgument
	}
	if err := ValidateAppealOperationKey(binding.IdempotencyKey); err != nil {
		return err
	}
	if err := ValidateAppealDigest(binding.RequestHash); err != nil {
		return err
	}
	return ValidateAppealDigest(binding.PolicyDigest)
}
