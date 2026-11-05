package knowledgeworker

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type renewalFailureStore struct {
	calls   atomic.Int32
	failure error
}

func (s *renewalFailureStore) ClaimLatest(context.Context) (*BuildClaim, error) { return nil, nil }
func (s *renewalFailureStore) Release(context.Context, BuildClaim) error        { return nil }
func (s *renewalFailureStore) Renew(context.Context, BuildClaim) error {
	if s.calls.Add(1) > 1 {
		return s.failure
	}
	return nil
}

func TestRunWithBuildLeasePreservesRenewalCause(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cause error
	}{
		{"obsolete", ErrBuildObsolete},
		{"lease_lost", ErrBuildLeaseLost},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := &renewalFailureStore{failure: tc.cause}
			ctx, cancel := context.WithTimeout(context.Background(), BuildRenewInterval+10*time.Second)
			defer cancel()
			err := RunWithBuildLease(ctx, store, BuildClaim{}, func(workCtx context.Context) error {
				<-workCtx.Done()
				return workCtx.Err()
			})
			if store.calls.Load() != 2 {
				t.Fatalf("renew calls = %d, want 2", store.calls.Load())
			}
			if !errors.Is(err, tc.cause) {
				t.Fatalf("got %v; want original renewal cause %v", err, tc.cause)
			}
		})
	}
}

func TestRunWithBuildLeasePreservesWorkFailure(t *testing.T) {
	want := errors.New("synthetic build failure")
	store := &renewalFailureStore{}
	err := RunWithBuildLease(context.Background(), store, BuildClaim{}, func(context.Context) error { return want })
	if !errors.Is(err, want) {
		t.Fatalf("got %v; want work error", err)
	}
}

func TestRunWithBuildLeaseSuccessfulWork(t *testing.T) {
	store := &renewalFailureStore{}
	if err := RunWithBuildLease(context.Background(), store, BuildClaim{}, func(context.Context) error { return nil }); err != nil {
		t.Fatalf("successful work returned %v", err)
	}
}
