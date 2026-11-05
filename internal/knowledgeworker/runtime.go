package knowledgeworker

import (
	"context"
	"errors"
	"time"
	"video-support-agent/internal/knowledgefailure"

	"go.uber.org/zap"
)

type RuntimeStore interface {
	BuildStore
	RecoveryStore

	Publish(context.Context, *VerifiedGeneration) error
	SettleReady(context.Context) (int, error)
}

type Builder interface {
	Build(context.Context, BuildClaim) (*VerifiedGeneration, error)
}

type Runtime struct {
	store   RuntimeStore
	builder Builder
	log     *zap.Logger
}

func NewRuntime(store RuntimeStore, builder Builder, log *zap.Logger) (*Runtime, error) {
	if store == nil || builder == nil || log == nil {
		return nil, ErrBuildConfig
	}
	return &Runtime{store: store, builder: builder, log: log}, nil
}

func runtimeWait(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (r *Runtime) RunBuilds(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		claim, err := r.store.ClaimLatest(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, ErrBuildCapacity) || errors.Is(err, ErrBuildExhausted) {
				code := "build_blocked"
				if errors.Is(err, ErrBuildCapacity) {
					code = "resource_capacity"
				}
				r.log.Warn("build waiting", zap.String("code", code))
				if err := runtimeWait(ctx, 5*time.Second); err != nil {
					return err
				}
				continue
			}
			return err
		}

		if claim == nil {
			if err := runtimeWait(ctx, time.Second); err != nil {
				return err
			}
			continue
		}

		if err := r.runClaim(ctx, *claim); err != nil {
			return err
		}
	}
}

func (r *Runtime) runClaim(ctx context.Context, claim BuildClaim) error {
	attemptCtx, cancel := context.WithTimeout(ctx, BuildTimeout)
	defer cancel()

	started := time.Now()
	verified, err := r.builder.Build(attemptCtx, claim)

	code := "verified"
	switch {
	case ctx.Err() != nil:
		code = "cancelled"
	case errors.Is(err, ErrBuildLeaseLost):
		code = "lease_lost"
	case errors.Is(err, ErrBuildObsolete):
		code = "obsolete"
	case err != nil:
		code = "build_unconfirmed"
		if confirmedCode, confirmed := confirmedFailureCode(err); confirmed {
			code = confirmedCode
		}
	}

	r.log.Info("knowledge build attempt",
		zap.String("generation_id", claim.GenerationID),
		zap.Uint64("source_epoch", claim.SourceEpoch),
		zap.Uint32("build_attempt", claim.Attempt),
		zap.String("code", code),
		zap.Float64("build_duration_seconds", time.Since(started).Seconds()))

	if err != nil {
		return r.handleBuildError(ctx, claim, err)
	}

	if err := r.store.Publish(attemptCtx, verified); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, ErrBuildObsolete) || errors.Is(err, ErrBuildLeaseLost) {
			r.log.Info("generation not selected",
				zap.String("generation_id", claim.GenerationID),
				zap.String("code", "generation_no_longer_owned"))
			return nil
		}

		r.log.Error("publication not confirmed",
			zap.String("generation_id", claim.GenerationID),
			zap.String("code", "publication_unconfirmed"))
		return ErrBuildStore
	}

	r.log.Info("generation published",
		zap.String("generation_id", claim.GenerationID),
		zap.Uint64("source_epoch", claim.SourceEpoch))
	return nil
}

func (r *Runtime) handleBuildError(ctx context.Context, claim BuildClaim, buildErr error) error {
	if ctx.Err() != nil {
		cleanupCtx, cancel := context.WithTimeout(
			context.WithoutCancel(ctx), 5*time.Second,
		)
		defer cancel()

		if err := r.store.Release(cleanupCtx, claim); err != nil {
			r.log.Warn("cancelled build release not confirmed",
				zap.String("generation_id", claim.GenerationID),
				zap.String("code", "release_unconfirmed"))
		}
		return ctx.Err()
	}

	if errors.Is(buildErr, ErrBuildLeaseLost) {
		return nil
	}
	if errors.Is(buildErr, ErrBuildObsolete) {
		return r.releaseObsolete(ctx, claim)
	}

	code, confirmed := confirmedFailureCode(buildErr)
	if !confirmed {
		return buildErr
	}

	err := r.recordFailureUntilKnown(ctx, claim, code)
	if errors.Is(err, ErrBuildObsolete) {
		return r.releaseObsolete(ctx, claim)
	}
	if errors.Is(err, ErrBuildLeaseLost) {
		return nil
	}
	if err != nil {
		return err
	}

	r.log.Warn("build failure recorded",
		zap.String("generation_id", claim.GenerationID),
		zap.Uint64("source_epoch", claim.SourceEpoch),
		zap.Uint32("build_attempt", claim.Attempt),
		zap.String("code", code))
	return nil
}

func (r *Runtime) releaseObsolete(ctx context.Context, claim BuildClaim) error {
	err := r.store.Release(ctx, claim)
	if errors.Is(err, ErrBuildLeaseLost) {
		return nil
	}
	return err
}

func confirmedFailureCode(err error) (string, bool) {
	for _, excluded := range []error{
		context.Canceled,
		ErrBuildConfig,
		ErrBuildState,
		ErrBuildStore,
		ErrBuildLeaseLost,
		ErrBuildObsolete,
		ErrBuildCapacity,
		ErrBuildExhausted,
	} {
		if errors.Is(err, excluded) {
			return "", false
		}
	}

	var code string
	switch {
	case errors.Is(err, ErrProjectionConflict):
		code = knowledgefailure.CodeProjectionConflict
	case errors.Is(err, ErrProjectionLimit):
		code = knowledgefailure.CodeProjectionLimit
	case errors.Is(err, ErrDualPermanent):
		code = knowledgefailure.CodeDualInputInvalid
	case errors.Is(err, ErrDualTimeout),
		errors.Is(err, context.DeadlineExceeded):
		code = knowledgefailure.CodeBuildTimeout
	case errors.Is(err, ErrDualExternal):
		code = knowledgefailure.CodeDualExternalFailed
	default:
		return "", false
	}

	_, known := knowledgefailure.Classify(code)
	return code, known
}

func (r *Runtime) RunSettlements(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		count, err := r.store.SettleReady(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}

		delay := 500 * time.Millisecond
		if count > 0 {
			delay = 25 * time.Millisecond
		}
		if err := runtimeWait(ctx, delay); err != nil {
			return err
		}
	}
}
