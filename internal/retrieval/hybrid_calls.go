package retrieval

import (
	"context"
	"errors"
	"time"
)

type hybridCallResult[T any] struct {
	value T
	err   error
}

func callHybridStage[T any](
	ctx context.Context,
	call func(context.Context) (T, error),
) (T, error) {
	var zero T

	if err := hybridContextError(ctx); err != nil {
		return zero, err
	}

	replies := make(chan hybridCallResult[T], 1)

	go func() {
		if err := hybridContextError(ctx); err != nil {
			replies <- hybridCallResult[T]{err: err}
			return
		}

		value, err := call(ctx)
		replies <- hybridCallResult[T]{
			value: value,
			err:   err,
		}
	}()

	select {
	case reply := <-replies:
		if err := hybridContextError(ctx); err != nil {
			return zero, err
		}
		if reply.err != nil {
			return zero, reply.err
		}
		return reply.value, nil

	case <-ctx.Done():
		return zero, ctx.Err()
	}
}

func hybridContextError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	if deadline, ok := ctx.Deadline(); ok &&
		!time.Now().Before(deadline) {
		return context.DeadlineExceeded
	}

	return nil
}

func makeHybridStageReport(
	started time.Time,
	err error,
) HybridStageReport {
	report := HybridStageReport{
		Status:    HybridStageSuccess,
		Duration:  time.Since(started),
		ErrorKind: classifyHybridError(err),
	}
	if err != nil {
		report.Status = HybridStageFailed
	}
	return report
}

func classifyHybridError(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, ErrHybridConfiguration):
		return "configuration"
	case errors.Is(err, ErrInvalidQuery):
		return "invalid_query"
	case errors.Is(err, ErrHybridUnavailable):
		return "unavailable"
	case errors.Is(err, ErrHydrationLimit):
		return "hydration_limit"
	case errors.Is(err, ErrInvalidRerank):
		return "invalid_rerank"
	case errors.Is(err, ErrHydrationFailed):
		return "hydration"
	case errors.Is(err, ErrInvalidCandidates):
		return "invalid_candidates"
	default:
		return "dependency"
	}
}
