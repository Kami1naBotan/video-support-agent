package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

const (
	workerProbeStarting uint32 = iota
	workerProbeRunning
	workerProbeDraining
)

type workerProbes struct {
	ctx    context.Context
	phase  atomic.Uint32
	server *http.Server
	errors chan error
}

func (p *workerProbes) MarkRunning() {
	if p.ctx.Err() == nil {
		p.phase.CompareAndSwap(workerProbeStarting, workerProbeRunning)
	}
}

func (p *workerProbes) BeginDrain() {
	p.phase.Store(workerProbeDraining)
}

func (p *workerProbes) Live(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

func (p *workerProbes) Ready(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")

	if p.ctx.Err() != nil || p.phase.Load() != workerProbeRunning {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ready\n"))
}

func startWorkerProbes(ctx context.Context, addr string) (*workerProbes, error) {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, errors.New("worker_probe_listen_failed")
	}

	p := &workerProbes{
		ctx:    ctx,
		errors: make(chan error, 1),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /livez", p.Live)
	mux.HandleFunc("GET /readyz", p.Ready)

	p.server = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 2 * time.Second,
		ReadTimeout:       3 * time.Second,
		WriteTimeout:      3 * time.Second,
		IdleTimeout:       15 * time.Second,
		MaxHeaderBytes:    8 * 1024,
	}

	go func() {
		if err := p.server.Serve(listener); err != nil &&
			!errors.Is(err, http.ErrServerClosed) {
			p.BeginDrain()
			p.errors <- errors.New("worker_probe_serve_failed")
		}
	}()

	return p, nil
}

func (p *workerProbes) Errors() <-chan error {
	return p.errors
}

func (p *workerProbes) Shutdown() error {
	p.BeginDrain()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := p.server.Shutdown(ctx); err != nil {
		_ = p.server.Close()
		return errors.New("worker_probe_shutdown_failed")
	}

	return nil
}
