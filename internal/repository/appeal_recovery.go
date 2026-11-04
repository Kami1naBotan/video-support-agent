package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"video-support-agent/internal/service"

	"github.com/go-sql-driver/mysql"
)

func appealStorageError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var mysqlErr *mysql.MySQLError
	if errors.As(err, &mysqlErr) && (mysqlErr.Number == 1213 || mysqlErr.Number == 1205) {
		return fmt.Errorf("%w: %w", service.ErrAppealRetryableStorage, err)
	}
	return fmt.Errorf("%w: %w", service.ErrAppealStorage, err)
}

func appealIdempotencyDuplicate(err error) bool {
	var mysqlErr *mysql.MySQLError
	if !errors.As(err, &mysqlErr) || mysqlErr.Number != 1062 {
		return false
	}
	const marker = " for key '"
	start := strings.LastIndex(mysqlErr.Message, marker)
	if start < 0 || !strings.HasSuffix(mysqlErr.Message, "'") {
		return false
	}
	key := mysqlErr.Message[start+len(marker) : len(mysqlErr.Message)-1]
	if dot := strings.LastIndexByte(key, '.'); dot >= 0 {
		key = key[dot+1:]
	}
	return key == "uk_appeal_ticket_user_key"
}

func appealRollback(ctx context.Context, cancel context.CancelFunc, tx *sql.Tx) error {
	cancel()
	finish, err := service.AppealFinalizationContext(ctx)
	if err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- tx.Rollback() }()
	select {
	case err := <-done:
		if errors.Is(err, sql.ErrTxDone) {
			return nil
		}
		return err
	case <-finish.Done():
		return finish.Err()
	}
}

func (r *AppealTicketRepository) recoverAppeal(ctx context.Context, command service.AppealCreateCommand, cause error) (*service.AppealCreateResult, error) {
	finish, err := service.AppealFinalizationContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", service.ErrAppealOutcomeUnknown, err)
	}
	ticket, err := r.GetByOperation(finish, command.UserID, command.IdempotencyKey)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", service.ErrAppealOutcomeUnknown, errors.Join(cause, err))
	}
	result, err := appealCreationResult(ticket, command, true)
	if errors.Is(err, service.ErrAppealIdempotencyConflict) {
		return nil, err
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %w", service.ErrAppealOutcomeUnknown, err)
	}
	return result, nil
}

func awaitAppealEnd(ctx context.Context, done <-chan error) (bool, error) {
	select {
	case err := <-done:
		return true, err
	case <-ctx.Done():
	}
	finish, err := service.AppealFinalizationContext(ctx)
	if err != nil {
		return false, err
	}
	select {
	case err := <-done:
		return true, err
	case <-finish.Done():
		select {
		case err := <-done:
			return true, err
		default:
			return false, finish.Err()
		}
	}
}
