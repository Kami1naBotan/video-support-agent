package knowledgeworker

import (
	"context"
	"errors"
	"go.uber.org/zap"
	"testing"
	"video-support-agent/internal/knowledgefailure"
)

type recoveryRuntimeProbe struct {
	RuntimeStore
	record  func(context.Context, BuildClaim, string) error
	adopt   func(context.Context) (bool, error)
	recover func(context.Context) (int, error)
}

func (p *recoveryRuntimeProbe) RecordBuildFailure(c context.Context, b BuildClaim, s string) error {
	return p.record(c, b, s)
}
func (p *recoveryRuntimeProbe) AdoptBlockedFailure(c context.Context) (bool, error) {
	return p.adopt(c)
}
func (p *recoveryRuntimeProbe) RecoverPending(c context.Context) (int, error) { return p.recover(c) }

type recoveryBuilderProbe struct{ calls int }

func (p *recoveryBuilderProbe) Build(context.Context, BuildClaim) (*VerifiedGeneration, error) {
	p.calls++
	return nil, ErrDualExternal
}

func TestRuntimeRetriesFailureRecordWithoutRebuilding(t *testing.T) {
	claim := BuildClaim{GenerationID: "generation", OwnerToken: "owner", SourceEpoch: 2, Attempt: 1}
	calls := 0
	store := &recoveryRuntimeProbe{record: func(_ context.Context, got BuildClaim, code string) error {
		calls++
		if got != claim || code != knowledgefailure.CodeDualExternalFailed {
			t.Fatalf("claim/code changed: %+v %s", got, code)
		}
		if calls == 1 {
			return ErrBuildStore
		}
		return nil
	}}
	builder := &recoveryBuilderProbe{}
	runtime, err := NewRuntime(store, builder, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.runClaim(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || builder.calls != 1 {
		t.Fatalf("records=%d builds=%d", calls, builder.calls)
	}
}

func TestRecoveryLoopStopsOnCancellationAndStoreFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	store := &recoveryRuntimeProbe{adopt: func(context.Context) (bool, error) { return false, nil }, recover: func(context.Context) (int, error) { calls++; cancel(); return 0, nil }}
	runtime := &Runtime{store: store, log: zap.NewNop()}
	if err := runtime.RunRecoveries(ctx); !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
	store.adopt = func(context.Context) (bool, error) { return false, ErrBuildStore }
	if err := runtime.RunRecoveries(context.Background()); !errors.Is(err, ErrBuildStore) || calls != 1 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}
