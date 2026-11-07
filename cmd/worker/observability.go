package main

import (
	"context"
	"errors"
	"time"

	"video-support-agent/internal/config"
	"video-support-agent/internal/observability"

	"go.uber.org/zap"
)

type workerObservation struct {
	Async   *observability.AsyncObserver
	Runtime *observability.Runtime
}

func runObservedWorker(ctx context.Context, cfg config.Config, log *zap.Logger) error {
	obs, err := observability.New(ctx, cfg.Observability, "worker")
	if err != nil {
		return err
	}
	defer func() {
		if err := obs.Shutdown(context.Background()); err != nil {
			log.Warn("worker observability shutdown incomplete")
		}
	}()

	metricsServer, err := observability.StartMetrics(obs)
	if err != nil {
		return err
	}
	defer func() {
		if err := metricsServer.Shutdown(context.Background()); err != nil {
			log.Warn("worker metrics shutdown incomplete")
		}
	}()

	asyncObserver, err := observability.NewAsyncObserver(obs)
	if err != nil {
		return err
	}

	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	result := make(chan error, 1)
	go func() {
		result <- runWorker(workCtx, cfg, log, workerObservation{
			Async:   asyncObserver,
			Runtime: obs,
		})
	}()

	var stopReason error

	select {
	case err := <-result:
		return err
	case err := <-metricsServer.Errors():
		stopReason = err
	case <-ctx.Done():
	}

	cancel()

	timer := time.NewTimer(65 * time.Second)
	defer timer.Stop()

	select {
	case err := <-result:
		if workerCancellationOnly(err) {
			err = nil
		}
		return errors.Join(stopReason, err)

	case <-timer.C:
		return errors.Join(stopReason, errors.New("worker observation shutdown timed out"))
	}
}
