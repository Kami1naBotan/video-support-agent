package knowledgeworker

import (
	"context"
	"time"

	"video-support-agent/internal/observability"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

type buildTraceReader interface {
	ReadBuildTraceParents(context.Context, BuildClaim) ([]string, bool, error)
}

func (r *Runtime) startBuildObservation(ctx context.Context, claim BuildClaim) (context.Context, func(string)) {
	if r.observer == nil {
		return ctx, func(string) {}
	}

	var parents []string
	available := false
	truncated := false

	if reader, ok := r.store.(buildTraceReader); ok {
		bounded, cancel := context.WithTimeout(ctx, time.Second)
		var err error
		parents, truncated, err = reader.ReadBuildTraceParents(bounded, claim)
		cancel()

		available = err == nil
		if err != nil {
			parents = nil
			truncated = false
		}
	}

	ctx, finish := r.observer.Start(ctx, "build", "", parents)

	trace.SpanFromContext(ctx).SetAttributes(attribute.String("app.generation_id", claim.GenerationID))

	observability.RecordAsyncLinkLoad(ctx, available, truncated)
	return ctx, finish
}
