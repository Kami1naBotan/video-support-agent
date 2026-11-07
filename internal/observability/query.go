package observability

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

type QueryObserver struct {
	backend  string
	tracer   trace.Tracer
	duration *prometheus.HistogramVec
}

func NewQueryObserver(runtime *Runtime, backend string) (*QueryObserver, error) {
	if runtime == nil {
		return nil, errors.New("query observation runtime is required")
	}

	var metricName string
	switch backend {
	case "elasticsearch":
		metricName = "es_query_duration_seconds"
	case "milvus":
		metricName = "milvus_query_duration_seconds"
	default:
		return nil, errors.New("unsupported query observation backend")
	}

	if !runtime.Enabled {
		return nil, nil
	}
	if runtime.Registry == nil || runtime.Provider == nil {
		return nil, errors.New("query observation dependencies are required")
	}

	duration := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: metricName,
			Help: "Search operation duration through response validation, in seconds.",
			Buckets: []float64{
				0.005, 0.01, 0.025, 0.05, 0.1,
				0.25, 0.5, 1, 2.5, 5, 10, 20, 40,
			},
		},
		[]string{"outcome"},
	)
	if err := runtime.Registry.Register(duration); err != nil {
		return nil, errors.New("register query duration metric failed")
	}

	return &QueryObserver{
		backend:  backend,
		tracer:   runtime.Provider.Tracer("video-support-agent/query"),
		duration: duration,
	}, nil
}

func (o *QueryObserver) Start(ctx context.Context) (context.Context, func(error)) {
	if o == nil {
		return ctx, func(error) {}
	}

	started := time.Now()
	work, span := o.tracer.Start(
		ctx,
		o.backend+".search",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("db.system", o.backend),
			attribute.String("db.operation.name", "search"),
		),
	)

	var once sync.Once
	finish := func(err error) {
		once.Do(func() {
			defer span.End()

			outcome := queryOutcome(err)
			span.SetAttributes(attribute.String("app.outcome", outcome))
			if outcome != "success" {
				span.SetStatus(codes.Error, outcome)
			}

			o.duration.WithLabelValues(outcome).Observe(time.Since(started).Seconds())
		})
	}

	return work, finish
}

func queryOutcome(err error) string {
	if err == nil {
		return "success"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timed_out"
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}

	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		return "timed_out"
	}
	return "failed"
}
