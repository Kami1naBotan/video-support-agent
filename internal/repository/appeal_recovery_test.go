package repository

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"video-support-agent/internal/service"

	"github.com/go-sql-driver/mysql"
)

func TestStorageClassification(t *testing.T) {
	for _, tc := range []struct {
		name      string
		number    uint16
		key       string
		duplicate bool
		category  error
	}{
		{"idempotency", 1062, "uk_appeal_ticket_user_key", true, service.ErrAppealStorage},
		{"qualified", 1062, "appeal_tickets.uk_appeal_ticket_user_key", true, service.ErrAppealStorage},
		{"primary", 1062, "PRIMARY", false, service.ErrAppealStorage},
		{"record", 1062, "uk_ticket_record_sequence", false, service.ErrAppealStorage},
		{"similar", 1062, "uk_appeal_ticket_user_key_extra", false, service.ErrAppealStorage},
		{"foreign_key", 1452, "uk_appeal_ticket_user_key", false, service.ErrAppealStorage},
		{"deadlock", 1213, "", false, service.ErrAppealRetryableStorage},
		{"lock_timeout", 1205, "", false, service.ErrAppealRetryableStorage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := &mysql.MySQLError{
				Number:  tc.number,
				Message: "Duplicate entry 'test' for key '" + tc.key + "'",
			}
			wrapped := fmt.Errorf("insert: %w", original)
			if got := appealIdempotencyDuplicate(wrapped); got != tc.duplicate {
				t.Fatalf("duplicate=%v", got)
			}
			err := appealStorageError(wrapped)
			if !errors.Is(err, tc.category) || !errors.Is(err, original) {
				t.Fatalf("category/cause=%v", err)
			}
		})
	}

	t.Run("malformed_duplicate", func(t *testing.T) {
		for _, message := range []string{
			"uk_appeal_ticket_user_key",
			"Duplicate entry 'uk_appeal_ticket_user_key' for key 'PRIMARY'",
			"for key 'uk_appeal_ticket_user_key",
		} {
			if appealIdempotencyDuplicate(
				&mysql.MySQLError{Number: 1062, Message: message},
			) {
				t.Fatalf("accepted malformed/unrelated: %s", message)
			}
		}
	})

	t.Run("context_and_nil", func(t *testing.T) {
		if appealStorageError(nil) != nil || appealIdempotencyDuplicate(nil) {
			t.Fatal("nil error changed")
		}
		for _, cause := range []error{
			context.Canceled, context.DeadlineExceeded,
		} {
			err := appealStorageError(fmt.Errorf("query: %w", cause))
			if !errors.Is(err, cause) || errors.Is(err, service.ErrAppealStorage) {
				t.Fatalf("context error=%v", err)
			}
		}
	})
}

func TestStorageAwait(t *testing.T) {
	t.Run("known_success", func(t *testing.T) {
		done := make(chan error, 1)
		done <- nil
		settled, err := awaitAppealEnd(context.Background(), done)
		if !settled || err != nil {
			t.Fatalf("settled=%v err=%v", settled, err)
		}
	})

	t.Run("known_error", func(t *testing.T) {
		cause := errors.New("test commit response lost")
		done := make(chan error, 1)
		done <- cause
		settled, err := awaitAppealEnd(context.Background(), done)
		if !settled || !errors.Is(err, cause) {
			t.Fatalf("settled=%v err=%v", settled, err)
		}
	})

	t.Run("canceled_known_success", func(t *testing.T) {
		parent, cancel := context.WithCancel(context.Background())
		ctx, release := service.WithAppealFinalization(parent)
		defer release()
		cancel()
		done := make(chan error, 1)
		done <- nil
		settled, err := awaitAppealEnd(ctx, done)
		if !settled || err != nil {
			t.Fatalf("known success erased: %v %v", settled, err)
		}
	})

	t.Run("expired_unknown", func(t *testing.T) {
		parent, cancel := context.WithDeadline(
			context.Background(), time.Now().Add(-time.Second),
		)
		defer cancel()
		ctx, release := service.WithAppealFinalization(parent)
		defer release()
		settled, err := awaitAppealEnd(ctx, make(chan error))
		if settled || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("settled=%v err=%v", settled, err)
		}
	})
}
