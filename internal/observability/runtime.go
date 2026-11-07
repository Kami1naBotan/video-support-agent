package observability

import (
	"context"
	"errors"
	"sync"
	"time"

	"video-support-agent/internal/config"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

const (
	exportTimeout   = 3 * time.Second
	shutdownTimeout = 5 * time.Second
)

type Runtime struct {
	Enabled     bool
	MetricsAddr string
	Registry    *prometheus.Registry
	Provider    trace.TracerProvider
	Propagator  propagation.TextMapPropagator

	provider    *sdktrace.TracerProvider
	closeOnce   sync.Once
	shutdownErr error
}

func New(ctx context.Context, cfg config.ObservabilityConfig, role string) (*Runtime, error) {
	if ctx == nil {
		return nil, errors.New("observability context is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if role != "api" && role != "worker" {
		return nil, errors.New("invalid observability role")
	}

	settings, err := config.ParseObservabilityConfig(cfg)
	if err != nil {
		return nil, err
	}

	runtime := &Runtime{
		Enabled:     settings.Enabled,
		MetricsAddr: settings.MetricsAddr,
		Registry:    prometheus.NewRegistry(),
		Provider:    noop.NewTracerProvider(),
		Propagator:  propagation.TraceContext{},
	}
	if !settings.Enabled {
		return runtime, nil
	}

	exporter, err := otlptracehttp.New(
		ctx,
		otlptracehttp.WithEndpointURL(settings.OTLPEndpoint),
		otlptracehttp.WithURLPath("/v1/traces"),
		otlptracehttp.WithHeaders(map[string]string{}),
		otlptracehttp.WithCompression(otlptracehttp.NoCompression),
		otlptracehttp.WithTimeout(exportTimeout),
		otlptracehttp.WithRetry(otlptracehttp.RetryConfig{Enabled: false}),
	)
	if err != nil {
		return nil, errors.New("initialize trace exporter failed")
	}

	provider := sdktrace.NewTracerProvider(
		sdktrace.WithResource(resource.NewSchemaless(
			attribute.String("service.name", settings.ServiceName+"-"+role),
		)),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(settings.SampleRatio))),
		sdktrace.WithBatcher(
			safeExporter{inner: exporter},
			sdktrace.WithMaxQueueSize(2048),
			sdktrace.WithMaxExportBatchSize(256),
			sdktrace.WithBatchTimeout(time.Second),
			sdktrace.WithExportTimeout(exportTimeout),
		),
	)

	runtime.Provider = provider
	runtime.provider = provider
	return runtime, nil
}

type safeExporter struct {
	inner sdktrace.SpanExporter
}

func (e safeExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	if err := e.inner.ExportSpans(ctx, spans); err != nil {
		return errors.New("trace export failed")
	}
	return nil
}

func (e safeExporter) Shutdown(ctx context.Context) error {
	if err := e.inner.Shutdown(ctx); err != nil {
		return errors.New("trace exporter shutdown failed")
	}
	return nil
}

func (r *Runtime) Shutdown(ctx context.Context) error {
	if r == nil {
		return nil
	}
	if ctx == nil {
		return errors.New("observability shutdown context is required")
	}

	r.closeOnce.Do(func() {
		if r.provider == nil {
			return
		}

		work, cancel := context.WithTimeout(ctx, shutdownTimeout)
		defer cancel()

		if err := r.provider.Shutdown(work); err != nil {
			r.shutdownErr = errors.New("observability shutdown failed")
		}
	})
	return r.shutdownErr
}
