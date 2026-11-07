package main

import (
	"context"
	"net/http"
	"sync/atomic"
	"time"
)

const readinessTimeout = 2 * time.Second

type apiProbes struct {
	draining atomic.Bool
	check    func(context.Context) error
}

func newAPIProbes(check func(context.Context) error) *apiProbes {
	return &apiProbes{check: check}
}

func (p *apiProbes) BeginDrain() {
	p.draining.Store(true)
}

func (p *apiProbes) Live(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

func (p *apiProbes) Ready(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")

	if p.draining.Load() || p.check == nil {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), readinessTimeout)
	defer cancel()

	if err := p.check(ctx); err != nil {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}

	if ctx.Err() != nil || p.draining.Load() {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ready\n"))
}
