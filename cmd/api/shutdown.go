package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

func shutdownHTTPServer(server *http.Server, timeout time.Duration) error {
	if server == nil {
		return errors.New("HTTP server is required")
	}
	if timeout <= 0 {
		return errors.New("shutdown timeout must be positive")
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		closeErr := server.Close()
		if closeErr != nil {
			return errors.Join(
				fmt.Errorf("HTTP graceful shutdown: %w", err),
				fmt.Errorf("HTTP forced close: %w", closeErr),
			)
		}

		return fmt.Errorf("HTTP graceful shutdown: %w", err)
	}

	return nil
}
