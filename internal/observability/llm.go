package observability

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

type LLMObserver struct {
	tracer   trace.Tracer
	duration *prometheus.HistogramVec
}

func NewLLMObserver(runtime *Runtime) (*LLMObserver, error) {
	if runtime == nil {
		return nil, errors.New("LLM observation runtime is required")
	}
	if !runtime.Enabled {
		return nil, nil
	}
	if runtime.Registry == nil || runtime.Provider == nil {
		return nil, errors.New("LLM observation dependencies are required")
	}

	duration := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "llm_call_duration_seconds",
			Help: "LLM HTTP request and response consumption duration in seconds.",
			Buckets: []float64{
				0.1, 0.25, 0.5, 1, 2.5,
				5, 10, 15, 20, 30, 40, 60,
			},
		},
		[]string{"role", "outcome"},
	)
	if err := runtime.Registry.Register(duration); err != nil {
		return nil, errors.New("register LLM duration metric failed")
	}

	return &LLMObserver{
		tracer:   runtime.Provider.Tracer("video-support-agent/llm"),
		duration: duration,
	}, nil
}

func (o *LLMObserver) HTTPClient(role string, timeout time.Duration) (*http.Client, error) {
	if role != "planner" && role != "answer" {
		return nil, errors.New("invalid LLM observation role")
	}
	if timeout <= 0 {
		return nil, errors.New("invalid LLM HTTP timeout")
	}

	var transport http.RoundTripper = http.DefaultTransport
	if o != nil {
		transport = &llmTransport{
			base:     transport,
			observer: o,
			role:     role,
		}
	}

	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
		CheckRedirect: func(
			req *http.Request,
			via []*http.Request,
		) error {
			return http.ErrUseLastResponse
		},
	}, nil
}

type llmTransport struct {
	base     http.RoundTripper
	observer *LLMObserver
	role     string
}

func (t *llmTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	started := time.Now()
	ctx, span := t.observer.tracer.Start(
		request.Context(),
		"llm."+t.role,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attribute.String("app.llm.role", t.role)),
	)

	var once sync.Once
	finish := func(outcome string) {
		once.Do(func() {
			defer span.End()

			span.SetAttributes(attribute.String("app.outcome", outcome))
			if outcome != "success" {
				span.SetStatus(codes.Error, outcome)
			}

			t.observer.duration.WithLabelValues(t.role, outcome).Observe(time.Since(started).Seconds())
		})
	}

	stopCancellation := context.AfterFunc(ctx, func() {
		finish(llmErrorOutcome(ctx, ctx.Err()))
	})
	end := func(outcome string) {
		stopCancellation()
		finish(outcome)
	}

	response, err := t.base.RoundTrip(request.Clone(ctx))
	if err != nil {
		end(llmErrorOutcome(ctx, err))
		return response, err
	}
	if response == nil || response.Body == nil {
		end("failed")
		return response, nil
	}

	httpFailed := response.StatusCode < 200 || response.StatusCode >= 300

	response.Body = &observedLLMBody{
		ReadCloser: response.Body,
		ctx:        ctx,
		httpFailed: httpFailed,
		finish:     end,
	}
	return response, nil
}

type observedLLMBody struct {
	io.ReadCloser
	ctx        context.Context
	httpFailed bool
	finish     func(string)
}

func (b *observedLLMBody) Read(buffer []byte) (int, error) {
	n, err := b.ReadCloser.Read(buffer)
	if err == nil {
		return n, nil
	}

	switch {
	case errors.Is(err, io.EOF):
		if b.httpFailed {
			b.finish("failed")
		} else {
			b.finish("success")
		}
	default:
		b.finish(llmErrorOutcome(b.ctx, err))
	}
	return n, err
}

func (b *observedLLMBody) Close() error {
	err := b.ReadCloser.Close()

	switch {
	case b.ctx.Err() != nil:
		b.finish(llmErrorOutcome(b.ctx, b.ctx.Err()))
	case err != nil:
		b.finish(llmErrorOutcome(b.ctx, err))
	case b.httpFailed:
		b.finish("failed")
	default:
		b.finish("closed")
	}
	return err
}

func llmErrorOutcome(ctx context.Context, err error) string {
	if contextErr := ctx.Err(); contextErr != nil {
		return queryOutcome(contextErr)
	}

	if deadline, ok := ctx.Deadline(); ok &&
		!time.Now().Before(deadline) {
		return "timed_out"
	}

	return queryOutcome(err)
}
