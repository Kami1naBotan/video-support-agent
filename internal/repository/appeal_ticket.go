package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"video-support-agent/internal/model"
	"video-support-agent/internal/service"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type AppealTicketRepository struct {
	db    *gorm.DB
	clock Clock
}

var _ service.AppealTicketStore = (*AppealTicketRepository)(nil)

func NewAppealTicketRepository(db *gorm.DB) (*AppealTicketRepository, error) {
	return NewAppealTicketRepositoryWithClock(db, time.Now)
}

func NewAppealTicketRepositoryWithClock(db *gorm.DB, clock Clock) (*AppealTicketRepository, error) {
	if db == nil || db.Error != nil || db.Statement == nil || clock == nil || db.Dialector == nil || db.Dialector.Name() != "mysql" || db.TranslateError || db.DryRun {
		return nil, service.ErrAppealConfiguration
	}

	if _, ok := db.Statement.ConnPool.(*sql.DB); !ok {
		return nil, service.ErrAppealConfiguration
	}

	return &AppealTicketRepository{
		db: db.Session(&gorm.Session{NewDB: true}), clock: clock,
	}, nil
}

func appealConversation(db *gorm.DB, userID, conversationID uint64, lock bool) error {
	if userID == 0 {
		return service.ErrUnauthorized
	}
	if conversationID == 0 {
		return service.ErrInvalidArgument
	}

	query := db.Select("id").Where("id = ? AND user_id = ?", conversationID, userID)
	if lock {
		query = query.Clauses(clause.Locking{Strength: "SHARE"})
	}

	var conversation model.Conversation
	err := query.Take(&conversation).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return service.ErrConversationNotFound
	}

	return appealStorageError(err)
}

func (r *AppealTicketRepository) CheckConversationOwnership(ctx context.Context, userID, conversationID uint64) error {
	return appealConversation(r.db.WithContext(ctx), userID, conversationID, false)
}

func appealFindOperation(db *gorm.DB, userID uint64, key string) (*model.AppealTicket, error) {
	if userID == 0 {
		return nil, service.ErrUnauthorized
	}

	if err := service.ValidateAppealOperationKey(key); err != nil {
		return nil, err
	}

	var ticket model.AppealTicket
	err := db.Where("user_id = ? AND idempotency_key = ?", userID, key).Take(&ticket).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, service.ErrAppealTicketNotFound
	}
	if err != nil {
		return nil, appealStorageError(err)
	}

	return &ticket, nil
}

func (r *AppealTicketRepository) GetByOperation(ctx context.Context, userID uint64, key string) (*model.AppealTicket, error) {
	return appealFindOperation(r.db.WithContext(ctx), userID, key)
}

func (r *AppealTicketRepository) GetByID(ctx context.Context, userID, ticketID uint64) (*model.AppealTicket, error) {
	if userID == 0 {
		return nil, service.ErrUnauthorized
	}
	if ticketID == 0 {
		return nil, service.ErrInvalidArgument
	}

	var ticket model.AppealTicket
	err := r.db.WithContext(ctx).Where("id = ? AND user_id = ?", ticketID, userID).Take(&ticket).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, service.ErrAppealTicketNotFound
	}
	if err != nil {
		return nil, appealStorageError(err)
	}

	return &ticket, nil
}

func normalizeAppealCommand(command service.AppealCreateCommand) (service.AppealCreateCommand, error) {
	if command.UserID == 0 {
		return command, service.ErrUnauthorized
	}
	if err := service.ValidateAppealOperationKey(command.IdempotencyKey); err != nil {
		return command, err
	}

	input, err := service.NormalizeCreateAppealInput(command.Input)
	if err != nil {
		return command, err
	}

	hash, err := service.AppealRequestHash(command.ConversationID, input)
	if err != nil {
		return command, err
	}
	if command.RequestHash != hash {
		return command, fmt.Errorf("%w: request_hash", service.ErrInvalidArgument)
	}
	command.Input = input
	return command, nil
}

func appealCreationResult(ticket *model.AppealTicket, command service.AppealCreateCommand, replayed bool) (*service.AppealCreateResult, error) {
	return service.AppealSavedCreationResult(ticket, command, replayed)
}
