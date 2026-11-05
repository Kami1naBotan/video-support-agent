package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"video-support-agent/internal/config"
	"video-support-agent/internal/logger"

	"go.uber.org/zap"
)

func main() {
	os.Exit(mainExit())
}

func mainExit() int {
	cfg := config.Load()

	log, err := logger.New(cfg.LogLevel)
	if err != nil {
		fmt.Fprintln(os.Stderr, "worker_logger_init_failed")
		return 1
	}
	defer func() {
		_ = log.Sync()
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := runWorker(ctx, cfg, log); err != nil {
		log.Error("worker stopped with error", zap.String("code", err.Error()))
		return 1
	}

	log.Info("worker stopped")
	return 0
}

func runWorker(ctx context.Context, cfg config.Config, log *zap.Logger) error {
	return runKnowledgeWorker(ctx, cfg, log)
}
