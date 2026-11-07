package observability

import (
	"context"
	"errors"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

const MaxAsyncLinks = 128

func NormalizeTraceParent(value string) string {
	if len(value) != 55 || value[:3] != "00-" || (value[53:] != "00" && value[53:] != "01") {
		return ""
	}

	carrier := propagation.MapCarrier{"traceparent": value}
	ctx := (propagation.TraceContext{}).Extract(context.Background(), carrier)
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return ""
	}

	canonical := propagation.MapCarrier{}
	(propagation.TraceContext{}).Inject(ctx, canonical)
	if canonical.Get("traceparent") != value {
		return ""
	}
	return value
}

func CaptureTraceParent(ctx context.Context, fallback ...string) string {
	carrier := propagation.MapCarrier{}
	(propagation.TraceContext{}).Inject(ctx, carrier)

	if value := NormalizeTraceParent(carrier.Get("traceparent")); value != "" {
		return value
	}
	for _, value := range fallback {
		if value = NormalizeTraceParent(value); value != "" {
			return value
		}
	}
	return ""
}

func remoteParent(ctx context.Context, parent string) context.Context {
	ctx = trace.ContextWithSpanContext(ctx, trace.SpanContext{})
	parent = NormalizeTraceParent(parent)
	if parent == "" {
		return ctx
	}

	return (propagation.TraceContext{}).Extract(
		ctx,
		propagation.MapCarrier{"traceparent": parent},
	)
}

type AsyncObserver struct {
	tracer trace.Tracer
}

func NewAsyncObserver(runtime *Runtime) (*AsyncObserver, error) {
	if runtime == nil {
		return nil, errors.New("async observation runtime is required")
	}
	if !runtime.Enabled {
		return nil, nil
	}
	if runtime.Provider == nil {
		return nil, errors.New("async observation provider is required")
	}
	return &AsyncObserver{
		tracer: runtime.Provider.Tracer("video-support-agent/async"),
	}, nil
}

func (o *AsyncObserver) Start(ctx context.Context, operation, parent string, parents []string) (context.Context, func(string)) {
	var kind trace.SpanKind
	switch operation {
	case "publish":
		kind = trace.SpanKindProducer
	case "receive":
		kind = trace.SpanKindConsumer
	case "build":
		kind = trace.SpanKindInternal
	default:
		return ctx, func(string) {}
	}

	ctx = remoteParent(ctx, parent)
	if o == nil {
		return ctx, func(string) {}
	}

	options := []trace.SpanStartOption{trace.WithSpanKind(kind)}
	if operation == "build" {
		options = append(options, trace.WithNewRoot())

		links := make([]trace.Link, 0, min(len(parents), MaxAsyncLinks))

		type linkIdentity struct {
			TraceID trace.TraceID
			SpanID  trace.SpanID
		}
		seen := make(map[linkIdentity]bool)

		for _, value := range parents {
			linked := remoteParent(context.Background(), value)
			sc := trace.SpanContextFromContext(linked)
			if !sc.IsValid() {
				continue
			}

			key := linkIdentity{
				TraceID: sc.TraceID(),
				SpanID:  sc.SpanID(),
			}
			if seen[key] {
				continue
			}

			seen[key] = true
			links = append(links, trace.Link{SpanContext: sc})
			if len(links) == MaxAsyncLinks {
				break
			}
		}

		options = append(options, trace.WithLinks(links...))
	}

	ctx, span := o.tracer.Start(ctx, "knowledge."+operation, options...)
	var once sync.Once

	return ctx, func(outcome string) {
		once.Do(func() {
			defer span.End()

			outcome = executionValue(outcome, "failed",
				"success", "failed", "timed_out", "cancelled",
				"unknown", "panic", "obsolete", "lease_lost",
				"published",
			)
			span.SetAttributes(attribute.String("app.outcome", outcome))
			switch outcome {
			case "failed", "timed_out", "unknown", "panic":
				span.SetStatus(codes.Error, outcome)
			}
		})
	}
}

func RecordAsyncLinkLoad(ctx context.Context, available, truncated bool) {
	trace.SpanFromContext(ctx).SetAttributes(
		attribute.Bool("app.source_links_available", available),
		attribute.Bool("app.source_links_truncated", truncated),
	)
}
