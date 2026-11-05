package knowledgeworker

import (
	"context"
	"errors"
	"time"

	"go.uber.org/zap"
)

func (r *Runtime) recordFailureUntilKnown(ctx context.Context, claim BuildClaim, code string) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		err := r.store.RecordBuildFailure(ctx, claim, code)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err == nil {
			return nil
		}
		if !errors.Is(err, ErrBuildStore) {
			return err
		}

		r.log.Warn("build failure persistence unconfirmed",
			zap.String("generation_id", claim.GenerationID),
			zap.Uint64("source_epoch", claim.SourceEpoch),
			zap.Uint32("build_attempt", claim.Attempt),
			zap.String("code", "failure_persistence_unconfirmed"))

		if err := runtimeWait(ctx, time.Second); err != nil {
			return err
		}
	}
}

func (r *Runtime) RunRecoveries(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		adopted, err := r.store.AdoptBlockedFailure(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}

		work, err := r.store.RecoverPending(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}

		if adopted {
			r.log.Info("legacy build failure adopted", zap.String("code", "recovery_recorded"))
		}
		if work > 0 {
			r.log.Info("build recovery batch committed", zap.Int("work_items", work))
		}

		delay := 500 * time.Millisecond
		if adopted || work > 0 {
			delay = 25 * time.Millisecond
		}
		if err := runtimeWait(ctx, delay); err != nil {
			return err
		}
	}
}
