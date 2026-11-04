package service

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestDay11Budget(t *testing.T) {
	t.Run("requires_installation", func(t *testing.T) {
		if _, err := AppealFinalizationContext(
			context.Background(),
		); !errors.Is(err, ErrAppealConfiguration) {
			t.Fatalf("error=%v", err)
		}
	})

	t.Run("shared_detached_and_released", func(t *testing.T) {
		parent, cancel := context.WithCancel(context.Background())
		defer cancel()
		ctx, release := WithAppealFinalization(parent)
		defer release()
		nested, nestedRelease := WithAppealFinalization(ctx)

		start := time.Now()
		first, err := AppealFinalizationContext(nested)
		if err != nil {
			t.Fatal(err)
		}
		deadline, ok := first.Deadline()
		if !ok || deadline.Before(start) ||
			deadline.After(time.Now().Add(3*time.Second)) {
			t.Fatal("unbounded deadline")
		}
		cancel()
		nestedRelease()
		if !errors.Is(ctx.Err(), context.Canceled) || first.Err() != nil {
			t.Fatal("working/cleanup cancellation mixed")
		}

		second, err := AppealFinalizationContext(ctx)
		if err != nil {
			t.Fatal(err)
		}
		secondDeadline, _ := second.Deadline()
		if !deadline.Equal(secondDeadline) {
			t.Fatal("cleanup budget restarted")
		}
		release()
		if !errors.Is(first.Err(), context.Canceled) ||
			!errors.Is(second.Err(), context.Canceled) {
			t.Fatal("cleanup not released")
		}
	})

	t.Run("earlier_deadline", func(t *testing.T) {
		cutoff := time.Now().Add(time.Second)
		parent, cancel := context.WithDeadline(context.Background(), cutoff)
		defer cancel()
		ctx, release := WithAppealFinalization(parent)
		defer release()
		finish, err := AppealFinalizationContext(ctx)
		if err != nil {
			t.Fatal(err)
		}
		deadline, ok := finish.Deadline()
		if !ok || !deadline.Equal(cutoff) {
			t.Fatal("original total deadline extended")
		}
	})

	t.Run("expired_deadline", func(t *testing.T) {
		parent, cancel := context.WithDeadline(
			context.Background(), time.Now().Add(-time.Second),
		)
		defer cancel()
		ctx, release := WithAppealFinalization(parent)
		defer release()
		finish, err := AppealFinalizationContext(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !errors.Is(finish.Err(), context.DeadlineExceeded) {
			t.Fatal("expired budget resurrected")
		}
	})
}
