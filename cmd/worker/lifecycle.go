package main

import (
	"context"
	"errors"
	"time"
)

type workerUnit struct {
	name string
	run  func(context.Context) error
}

type workerUnitResult struct {
	name string
	err  error
}

func workerCancellationOnly(err error) bool {
	if err == nil {
		return false
	}

	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		found := false

		for _, child := range joined.Unwrap() {
			if child == nil {
				continue
			}
			found = true

			if !workerCancellationOnly(child) {
				return false
			}
		}

		return found
	}

	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return workerCancellationOnly(wrapped.Unwrap())
	}

	return err == context.Canceled
}

func runWorkerUnits(ctx context.Context, units []workerUnit) (time.Time, error) {
	return runWorkerUnitsWithProbes(ctx, units, nil)
}

func runWorkerUnitsWithProbes(ctx context.Context, units []workerUnit, probes *workerProbes) (time.Time, error) {
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	if probes != nil {
		defer probes.BeginDrain()
	}

	results := make(chan workerUnitResult, len(units))
	for _, unit := range units {
		go func(unit workerUnit) {
			results <- workerUnitResult{
				name: unit.name,
				err:  unit.run(workCtx),
			}
		}(unit)
	}

	if probes != nil {
		probes.MarkRunning()
	}

	remaining := len(units)
	var runErr error

	select {
	case <-ctx.Done():
	case result := <-results:
		remaining--
		if ctx.Err() == nil {
			runErr = errors.New(result.name + "_stopped")
		} else if result.err != nil && !workerCancellationOnly(result.err) {
			runErr = errors.New(result.name + "_failed")
		}
	}

	if probes != nil {
		probes.BeginDrain()
	}

	deadline := time.Now().Add(30 * time.Second)
	cancel()

	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()

	for remaining > 0 {
		select {
		case result := <-results:
			remaining--
			if runErr == nil && result.err != nil && !workerCancellationOnly(result.err) {
				runErr = errors.New(result.name + "_failed")
			}
		case <-timer.C:
			return deadline, errors.New("worker_shutdown_timeout")
		}
	}
	return deadline, runErr
}

type workerCleanup func(context.Context) error

func closeWorkerResources(deadline time.Time, cleanups []workerCleanup) error {
	if deadline.IsZero() {
		deadline = time.Now().Add(30 * time.Second)
	}
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		var result error
		for i := len(cleanups) - 1; i >= 0; i-- {
			if ctx.Err() != nil {
				done <- errors.New("worker_cleanup_timeout")
				return
			}
			if err := cleanups[i](ctx); err != nil {
				result = errors.New("worker_cleanup_failed")
			}
		}
		done <- result
	}()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return errors.New("worker_cleanup_timeout")
	}
}
