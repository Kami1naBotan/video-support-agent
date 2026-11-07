package main

import (
	"context"
	"errors"
	"go.uber.org/zap"
	"testing"
	"time"
	"video-support-agent/internal/knowledgeworker"
)

func TestKnowledgeLoopRetryContract(t *testing.T) {
	t.Run("persistence recovers", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		calls := 0
		err := retryKnowledgeLoop(ctx, "test", func(context.Context) error {
			calls++
			if calls == 1 {
				return knowledgeworker.ErrBuildStore
			}
			return nil
		}, zap.NewNop())
		if err != nil || calls != 2 {
			t.Fatalf("err=%v calls=%d", err, calls)
		}
	})
	t.Run("state error stops", func(t *testing.T) {
		calls := 0
		err := retryKnowledgeLoop(context.Background(), "test", func(context.Context) error { calls++; return knowledgeworker.ErrBuildState }, zap.NewNop())
		if !errors.Is(err, knowledgeworker.ErrBuildState) || calls != 1 {
			t.Fatalf("err=%v calls=%d", err, calls)
		}
	})
	t.Run("cancel interrupts backoff", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		entered := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			done <- retryKnowledgeLoop(ctx, "test", func(context.Context) error { close(entered); return knowledgeworker.ErrBuildStore }, zap.NewNop())
		}()
		<-entered
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		case <-time.After(500 * time.Millisecond):
			t.Fatal("cancellation delayed")
		}
	})
}
