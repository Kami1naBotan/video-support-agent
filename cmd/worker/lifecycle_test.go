package main

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// Consumer.Run joins its run error with the client Close error.
// Exercise that exact error shape through the real unit supervisor.
func TestWorkerShutdownPreservesJoinedFailure(t *testing.T) {
	for _, tc := range []struct {
		name        string
		result      error
		wantFailure bool
	}{
		{"cancel_only", context.Canceled, false},
		{"wrapped_cancel", fmt.Errorf("wrapped: %w", context.Canceled), false},
		{"joined_cancel_only", errors.Join(context.Canceled, context.Canceled), false},
		{"cancel_and_close_failure", errors.Join(context.Canceled, errors.New("secret close detail")), true},
		{"nested_close_failure", fmt.Errorf("wrapped: %w", errors.Join(context.Canceled, errors.New("secret close detail"))), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			_, err := runWorkerUnits(ctx, []workerUnit{{name: "retry_consumer", run: func(ctx context.Context) error { <-ctx.Done(); return tc.result }}})
			if tc.wantFailure {
				if err == nil || err.Error() != "retry_consumer_failed" {
					t.Fatalf("got %v; want safe retry_consumer_failed", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}
