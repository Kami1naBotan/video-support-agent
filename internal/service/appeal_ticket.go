package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"video-support-agent/internal/model"
)

type AppealTicketService struct {
	store         AppealTicketStore
	eligibility   *AppealEligibilityService
	confirmations AppealConfirmations
	clock         func() time.Time
}

func NewAppealTicketService(store AppealTicketStore, eligibility *AppealEligibilityService, confirmations AppealConfirmations) (*AppealTicketService, error) {
	return NewAppealTicketServiceWithClock(store, eligibility, confirmations, time.Now)
}

func NewAppealTicketServiceWithClock(store AppealTicketStore, eligibility *AppealEligibilityService, confirmations AppealConfirmations, clock func() time.Time) (*AppealTicketService, error) {
	if store == nil || eligibility == nil || eligibility.policies == nil || clock == nil {
		return nil, ErrAppealConfiguration
	}
	return &AppealTicketService{
		store:         store,
		eligibility:   eligibility,
		confirmations: confirmations,
		clock:         clock,
	}, nil
}

func (s *AppealTicketService) authorize(ctx context.Context, userID, conversationID uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if userID == 0 {
		return ErrUnauthorized
	}
	if conversationID == 0 {
		return ErrInvalidArgument
	}
	if s == nil || s.store == nil {
		return ErrAppealConfiguration
	}
	return s.store.CheckConversationOwnership(ctx, userID, conversationID)
}

func appealFactsFromInput(input CreateAppealInput) AppealFacts {
	return AppealFacts{
		SubjectType: input.SubjectType,
		SubjectRef:  input.SubjectRef,
		Reason:      input.Reason,
	}
}

func (s *AppealTicketService) CheckEligibility(ctx context.Context, userID, conversationID uint64, raw AppealFacts) (model.AppealEligibilitySnapshot, error) {
	ctx, release := WithAppealFinalization(ctx)
	defer release()
	if err := s.authorize(ctx, userID, conversationID); err != nil {
		return model.AppealEligibilitySnapshot{}, err
	}
	return s.eligibility.Evaluate(ctx, raw, s.clock(), s.store)
}

func (s *AppealTicketService) Prepare(ctx context.Context, userID, conversationID uint64, raw CreateAppealInput) (*PreparedAppeal, error) {
	ctx, release := WithAppealFinalization(ctx)
	defer release()
	if err := s.authorize(ctx, userID, conversationID); err != nil {
		return nil, err
	}
	input, err := NormalizeCreateAppealInput(raw)
	if err != nil {
		return nil, err
	}
	if s.confirmations == nil {
		return nil, ErrAppealConfiguration
	}
	issuedAt := s.clock().UTC().Truncate(time.Millisecond)
	expiresAt := issuedAt.Add(AppealConfirmationLifetime)
	if !validAppealTime(issuedAt) || !validAppealTime(expiresAt) {
		return nil, ErrAppealConfiguration
	}
	snapshot, err := s.eligibility.Evaluate(
		ctx, appealFactsFromInput(input), issuedAt, s.store,
	)
	if err != nil {
		return nil, err
	}
	status, err := AppealInitialStatus(snapshot, input.IntakeMode)
	if err != nil {
		return nil, err
	}
	policyDigest, err := AppealPolicyDigest(snapshot)
	if err != nil {
		return nil, err
	}
	requestHash, err := AppealRequestHash(conversationID, input)
	if err != nil {
		return nil, err
	}
	key, err := newAppealOperationKey()
	if err != nil {
		return nil, err
	}
	binding := AppealConfirmationBinding{
		UserID: userID, ConversationID: conversationID,
		IdempotencyKey: key,
		RequestHash:    requestHash,
		PolicyDigest:   policyDigest,
	}
	if err := ValidateAppealConfirmationBinding(binding); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	token, err := s.confirmations.Issue(ctx, binding, issuedAt, expiresAt)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !validAppealConfirmationToken(token) {
		return nil, ErrAppealConfiguration
	}
	return &PreparedAppeal{
		Input:         input,
		Eligibility:   snapshot,
		InitialStatus: status,
		Binding:       binding,
		Token:         token,
		IssuedAt:      issuedAt,
		ExpiresAt:     expiresAt,
	}, nil
}

func (s *AppealTicketService) operation(ctx context.Context, userID, conversationID uint64, key string, raw CreateAppealInput) (AppealCreateCommand, error) {
	var command AppealCreateCommand
	if err := s.authorize(ctx, userID, conversationID); err != nil {
		return command, err
	}
	if err := ValidateAppealOperationKey(key); err != nil {
		return command, err
	}
	input, err := NormalizeCreateAppealInput(raw)
	if err != nil {
		return command, err
	}
	hash, err := AppealRequestHash(conversationID, input)
	if err != nil {
		return command, err
	}
	return AppealCreateCommand{
		UserID:         userID,
		ConversationID: conversationID,
		IdempotencyKey: key,
		RequestHash:    hash,
		Input:          input,
	}, nil
}

func (s *AppealTicketService) replay(ctx context.Context, command AppealCreateCommand) (*AppealCreateResult, error) {
	ticket, err := s.store.GetByOperation(
		ctx, command.UserID, command.IdempotencyKey,
	)
	if err != nil {
		return nil, err
	}
	return AppealSavedCreationResult(ticket, command, true)
}

func (s *AppealTicketService) Create(ctx context.Context, userID, conversationID uint64, raw CreateAppealInput, confirmation AppealConfirmationInput) (*AppealCreateResult, error) {
	ctx, release := WithAppealFinalization(ctx)
	defer release()
	command, err := s.operation(
		ctx, userID, conversationID, confirmation.IdempotencyKey, raw,
	)
	if err != nil {
		return nil, err
	}
	result, err := s.replay(ctx, command)
	if err == nil {
		return result, nil
	}
	if !errors.Is(err, ErrAppealTicketNotFound) {
		return nil, err
	}
	command.ExpectedPolicyDigest = confirmation.PolicyDigest
	binding := AppealConfirmationBinding{
		UserID: userID, ConversationID: conversationID,
		IdempotencyKey: command.IdempotencyKey,
		RequestHash:    command.RequestHash,
		PolicyDigest:   command.ExpectedPolicyDigest,
	}
	if err := s.verifyConfirmation(ctx, confirmation.Token, binding); err != nil {
		return nil, err
	}
	facts := appealFactsFromInput(command.Input)
	result, err = s.store.CreateWithRecord(ctx, command,
		func(txCtx context.Context, checker AppealEvidenceChecker, at time.Time) (model.AppealEligibilitySnapshot, error) {
			snapshot, err := s.eligibility.Evaluate(txCtx, facts, at, checker)
			if err != nil {
				return model.AppealEligibilitySnapshot{}, err
			}
			digest, err := AppealPolicyDigest(snapshot)
			if err != nil {
				return model.AppealEligibilitySnapshot{}, err
			}
			if digest != command.ExpectedPolicyDigest {
				return model.AppealEligibilitySnapshot{}, ErrAppealPreconditionChanged
			}
			if _, err := AppealInitialStatus(snapshot, command.Input.IntakeMode); err != nil {
				return model.AppealEligibilitySnapshot{}, err
			}
			if err := s.verifyConfirmation(txCtx, confirmation.Token, binding); err != nil {
				return model.AppealEligibilitySnapshot{}, err
			}
			return snapshot, nil
		})
	if err != nil {
		return nil, err
	}
	expectedStatus := model.AppealTicketSubmitted
	if command.Input.IntakeMode == model.AppealIntakeManualReview {
		expectedStatus = model.AppealTicketPendingReview
	}
	if result == nil || result.TicketID == 0 || !validAppealTime(result.CreatedAt) || result.InitialStatus != expectedStatus {
		return nil, ErrAppealOutcomeUnknown
	}
	return result, nil
}

func (s *AppealTicketService) Recover(ctx context.Context, userID, conversationID uint64, key, requestHash string) (*AppealCreateResult, error) {
	if err := s.authorize(ctx, userID, conversationID); err != nil {
		return nil, err
	}
	if err := ValidateAppealOperationKey(key); err != nil {
		return nil, err
	}
	if err := ValidateAppealDigest(requestHash); err != nil {
		return nil, err
	}
	command := AppealCreateCommand{
		UserID: userID, ConversationID: conversationID,
		IdempotencyKey: key, RequestHash: requestHash,
	}
	result, err := s.replay(ctx, command)
	if err == nil || errors.Is(err, ErrAppealIdempotencyConflict) {
		return result, err
	}
	return nil, fmt.Errorf("%w: %w", ErrAppealOutcomeUnknown, err)
}

func (s *AppealTicketService) Query(ctx context.Context, userID, conversationID, ticketID uint64) (*AppealTicketView, error) {
	if err := s.authorize(ctx, userID, conversationID); err != nil {
		return nil, err
	}
	if ticketID == 0 {
		return nil, ErrInvalidArgument
	}
	ticket, err := s.store.GetByID(ctx, userID, ticketID)
	if err != nil {
		return nil, err
	}
	if ticket == nil {
		return nil, ErrAppealStorage
	}
	if ticket.UserID != userID {
		return nil, ErrAppealTicketNotFound
	}
	switch ticket.Status {
	case model.AppealTicketSubmitted, model.AppealTicketPendingReview:
	default:
		return nil, ErrAppealStorage
	}
	if ticket.ID != ticketID || !validAppealTime(ticket.CreatedAt) ||
		!validAppealTime(ticket.UpdatedAt) ||
		ticket.UpdatedAt.Before(ticket.CreatedAt) {
		return nil, ErrAppealStorage
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &AppealTicketView{
		TicketID:    ticket.ID,
		SubjectType: ticket.SubjectType, SubjectRef: ticket.SubjectRef,
		Status:    ticket.Status,
		CreatedAt: ticket.CreatedAt.UTC(),
		UpdatedAt: ticket.UpdatedAt.UTC(),
	}, nil
}
