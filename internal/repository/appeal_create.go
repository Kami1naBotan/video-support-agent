package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"video-support-agent/internal/model"
	"video-support-agent/internal/service"

	"gorm.io/gorm/clause"
)

func (r *AppealTicketRepository) CreateWithRecord(ctx context.Context, command service.AppealCreateCommand, check service.AppealTransactionCheck) (*service.AppealCreateResult, error) {
	ctx, release := service.WithAppealFinalization(ctx)
	defer release()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	command, err := normalizeAppealCommand(command)
	if err != nil {
		return nil, err
	}
	if err := r.CheckConversationOwnership(ctx, command.UserID, command.ConversationID); err != nil {
		return nil, err
	}
	existing, err := r.GetByOperation(ctx, command.UserID, command.IdempotencyKey)
	if err == nil {
		return appealCreationResult(existing, command, true)
	}
	if !errors.Is(err, service.ErrAppealTicketNotFound) {
		return nil, err
	}
	if err := service.ValidateAppealDigest(command.ExpectedPolicyDigest); err != nil {
		return nil, err
	}
	if check == nil {
		return nil, service.ErrAppealConfiguration
	}

	workCtx, cancelWork := context.WithCancel(ctx)
	defer cancelWork()
	tx := r.db.WithContext(workCtx).Begin(&sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if tx.Error != nil {
		return nil, appealStorageError(tx.Error)
	}
	sqlTx := tx.Statement.ConnPool.(*sql.Tx)
	closed := false
	closeTransaction := func() error {
		err := appealRollback(ctx, cancelWork, sqlTx)
		closed = true
		return err
	}
	defer func() {
		if !closed {
			_ = closeTransaction()
		}
	}()
	abort := func(cause error) (*service.AppealCreateResult, error) {
		if cleanupErr := closeTransaction(); cleanupErr != nil {
			return nil, fmt.Errorf("%w: %w", service.ErrAppealStorage, errors.Join(cause, cleanupErr))
		}
		return nil, cause
	}
	if err := appealConversation(tx, command.UserID, command.ConversationID, true); err != nil {
		return abort(err)
	}
	existing, err = appealFindOperation(tx, command.UserID, command.IdempotencyKey)
	if err == nil {
		result, resultErr := appealCreationResult(existing, command, true)
		if cleanupErr := closeTransaction(); cleanupErr != nil {
			return nil, fmt.Errorf("%w: %w", service.ErrAppealStorage, errors.Join(resultErr, cleanupErr))
		}
		return result, resultErr
	}
	if !errors.Is(err, service.ErrAppealTicketNotFound) {
		return abort(err)
	}

	now := r.clock().UTC().Truncate(time.Millisecond)
	checker := &appealTxEvidence{db: tx}
	snapshot, err := check(workCtx, checker, now)
	if err != nil {
		return abort(err)
	}
	snapshot, err = service.NormalizeAppealSnapshot(snapshot)
	if err != nil {
		return abort(err)
	}
	if !snapshot.CheckedAt.Equal(now) {
		return abort(service.ErrAppealInvalidSnapshot)
	}
	digest, err := service.AppealPolicyDigest(snapshot)
	if err != nil {
		return abort(err)
	}
	if digest != command.ExpectedPolicyDigest {
		return abort(service.ErrAppealPreconditionChanged)
	}
	status, err := service.AppealInitialStatus(snapshot, command.Input.IntakeMode)
	if err != nil {
		return abort(err)
	}
	if len(snapshot.EvidenceRefs) != 0 {
		available, err := checker.CheckAppealEvidence(workCtx, snapshot.EvidenceRefs, now)
		if err != nil {
			return abort(err)
		}
		if snapshot.Decision == model.AppealEligibilityEligible && !available {
			return abort(service.ErrAppealPreconditionChanged)
		}
	}
	if err := workCtx.Err(); err != nil {
		return abort(err)
	}
	ticket := model.AppealTicket{
		UserID:              command.UserID,
		ConversationID:      command.ConversationID,
		SubjectType:         command.Input.SubjectType,
		SubjectRef:          command.Input.SubjectRef,
		Reason:              command.Input.Reason,
		IntakeMode:          command.Input.IntakeMode,
		Status:              status,
		IdempotencyKey:      command.IdempotencyKey,
		RequestHash:         command.RequestHash,
		EligibilitySnapshot: snapshot,
		CreatedAt:           now,
		UpdatedAt:           now,
	}
	if err := tx.Omit(clause.Associations).Create(&ticket).Error; err != nil {
		duplicate := appealIdempotencyDuplicate(err)
		if cleanupErr := closeTransaction(); cleanupErr != nil {
			return nil, fmt.Errorf("%w: %w", service.ErrAppealStorage, errors.Join(err, cleanupErr))
		}
		if duplicate {
			return r.recoverAppeal(ctx, command, err)
		}
		return nil, appealStorageError(err)
	}
	reasonCode := "eligible_precheck"
	if status == model.AppealTicketPendingReview {
		reasonCode = "manual_review_requested"
	}
	record := model.TicketRecord{
		TicketID:    ticket.ID,
		Sequence:    1,
		Action:      model.TicketRecordCreated,
		ActorUserID: command.UserID,
		FromStatus:  nil,
		ToStatus:    status,
		ReasonCode:  reasonCode,
		CreatedAt:   now,
	}
	if err := tx.Omit(clause.Associations).Create(&record).Error; err != nil {
		return abort(appealStorageError(err))
	}
	result, err := appealCreationResult(&ticket, command, false)
	if err != nil {
		return abort(err)
	}
	if err := workCtx.Err(); err != nil {
		return abort(err)
	}
	committed := make(chan error, 1)
	go func() { committed <- sqlTx.Commit() }()
	settled, commitErr := awaitAppealEnd(ctx, committed)
	if !settled {
		closed = true
		return nil, fmt.Errorf("%w: %w", service.ErrAppealOutcomeUnknown, commitErr)
	}
	if err := commitErr; err != nil {
		if cleanupErr := closeTransaction(); cleanupErr != nil {
			return nil, fmt.Errorf("%w: %w", service.ErrAppealOutcomeUnknown, errors.Join(err, cleanupErr))
		}
		return r.recoverAppeal(ctx, command, err)
	}
	closed = true
	return result, nil
}
