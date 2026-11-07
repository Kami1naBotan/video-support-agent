package observability

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type MetricsServer struct {
	server *http.Server
	errors chan error
	done   chan struct{}
}

func StartMetrics(runtime *Runtime) (*MetricsServer, error) {
	if runtime == nil {
		return nil, errors.New("metrics runtime is required")
	}
	if !runtime.Enabled {
		return &MetricsServer{}, nil
	}
	if runtime.Registry == nil {
		return nil, errors.New("metrics registry is required")
	}

	listener, err := net.Listen("tcp", runtime.MetricsAddr)
	if err != nil {
		return nil, errors.New("metrics listen failed")
	}

	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(
		runtime.Registry,
		promhttp.HandlerOpts{
			MaxRequestsInFlight: 2,
			Timeout:             3 * time.Second,
		},
	))

	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 2 * time.Second,
		ReadTimeout:       3 * time.Second,
		WriteTimeout:      5 * time.Second,
		IdleTimeout:       10 * time.Second,
		MaxHeaderBytes:    8 * 1024,
		ErrorLog:          log.New(io.Discard, "", 0),
	}

	result := &MetricsServer{
		server: server,
		errors: make(chan error, 1),
		done:   make(chan struct{}),
	}
	go result.serve(listener)
	return result, nil
}

func (s *MetricsServer) serve(listener net.Listener) {
	defer close(s.done)

	err := s.server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return
	}

	s.errors <- errors.New("metrics server stopped unexpectedly")
}

func (s *MetricsServer) Errors() <-chan error {
	if s == nil {
		return nil
	}
	return s.errors
}

func (s *MetricsServer) Shutdown(ctx context.Context) error {
	if s == nil || s.server == nil {
		return nil
	}
	if ctx == nil {
		return errors.New("metrics shutdown context is required")
	}

	work, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if err := s.server.Shutdown(work); err != nil {
		_ = s.server.Close()
		return errors.New("metrics shutdown failed")
	}

	select {
	case <-s.done:
		return nil
	case <-work.Done():
		_ = s.server.Close()
		return errors.New("metrics shutdown timed out")
	}
}
