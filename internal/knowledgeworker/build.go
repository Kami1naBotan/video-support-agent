package knowledgeworker

import (
	"context"
	"errors"
	"time"
)

const (
	BuildLeaseDuration = 90 * time.Second
	BuildRenewInterval = 20 * time.Second
	BuildTimeout       = 5 * time.Minute
)

var (
	ErrBuildConfig    = errors.New("invalid build configuration")
	ErrBuildState     = errors.New("build state inconsistent")
	ErrBuildStore     = errors.New("build persistence unconfirmed")
	ErrBuildLeaseLost = errors.New("build lease lost")
	ErrBuildObsolete  = errors.New("build epoch obsolete")
	ErrBuildCapacity  = errors.New("build resource capacity reached")
	ErrBuildExhausted = errors.New("build attempts exhausted")
)

type BuildClaim struct {
	GenerationID     string
	SourceEpoch      uint64
	OwnerToken       string
	Attempt          uint32
	ESIndexName      string
	MilvusCollection string
}

type BuildStore interface {
	ClaimLatest(context.Context) (*BuildClaim, error)
	Renew(context.Context, BuildClaim) error
	Release(context.Context, BuildClaim) error
}

func RunWithBuildLease(ctx context.Context, store BuildStore, claim BuildClaim, work func(context.Context) error) error {
	if store == nil || work == nil {
		return ErrBuildConfig
	}

	budgetCtx, cancelBudget := context.WithTimeout(ctx, BuildTimeout)
	defer cancelBudget()

	workCtx, cancelWork := context.WithCancelCause(budgetCtx)
	defer cancelWork(context.Canceled)

	if err := store.Renew(workCtx, claim); err != nil {
		return err
	}

	done := make(chan struct{})
	var renewalErr error

	go func() {
		defer close(done)

		ticker := time.NewTicker(BuildRenewInterval)
		defer ticker.Stop()

		for {
			select {
			case <-workCtx.Done():
				return
			case <-ticker.C:
				if err := store.Renew(workCtx, claim); err != nil {
					if workCtx.Err() == nil {
						renewalErr = err
						cancelWork(err)
					}
					return
				}
			}
		}
	}()

	workErr := work(workCtx)
	cause := context.Cause(workCtx)

	cancelWork(context.Canceled)
	<-done

	if renewalErr != nil {
		return renewalErr
	}
	if cause != nil {
		return cause
	}
	return workErr
}
