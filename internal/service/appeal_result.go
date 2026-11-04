package service

import (
	"fmt"
	"time"

	"video-support-agent/internal/model"
)

func validAppealTime(at time.Time) bool {
	year := at.UTC().Year()
	return !at.IsZero() && year >= 1000 && year <= 9999 &&
		at.Equal(at.Truncate(time.Millisecond))
}

func AppealSavedCreationResult(ticket *model.AppealTicket, command AppealCreateCommand, replayed bool) (*AppealCreateResult, error) {
	bad := func() (*AppealCreateResult, error) {
		return nil, fmt.Errorf("%w: invalid saved creation", ErrAppealStorage)
	}
	if ticket == nil {
		return bad()
	}
	if ticket.UserID != command.UserID {
		return nil, ErrAppealTicketNotFound
	}
	if ticket.IdempotencyKey != command.IdempotencyKey {
		return bad()
	}
	if ticket.RequestHash != command.RequestHash {
		return nil, ErrAppealIdempotencyConflict
	}

	input := CreateAppealInput{
		SubjectType: ticket.SubjectType,
		SubjectRef:  ticket.SubjectRef,
		Reason:      ticket.Reason,
		IntakeMode:  ticket.IntakeMode,
	}

	normalized, err := NormalizeCreateAppealInput(input)
	if err != nil || normalized != input {
		return bad()
	}

	hash, err := AppealRequestHash(ticket.ConversationID, input)
	if err != nil || hash != ticket.RequestHash || ticket.ID == 0 ||
		ticket.ConversationID != command.ConversationID ||
		!validAppealTime(ticket.CreatedAt) ||
		!ticket.EligibilitySnapshot.CheckedAt.Equal(ticket.CreatedAt) {
		return bad()
	}
	if err := ValidateAppealOperationKey(ticket.IdempotencyKey); err != nil {
		return bad()
	}

	initialStatus, err := AppealInitialStatus(ticket.EligibilitySnapshot, ticket.IntakeMode)
	if err != nil {
		return bad()
	}
	return &AppealCreateResult{
		TicketID:      ticket.ID,
		InitialStatus: initialStatus,
		CreatedAt:     ticket.CreatedAt.UTC(),
		Replayed:      replayed,
	}, nil
}
