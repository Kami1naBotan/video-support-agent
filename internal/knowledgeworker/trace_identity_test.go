package knowledgeworker

import (
	"context"
	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"testing"
	"video-support-agent/internal/observability"
)

func TestBuildObservationGenerationIdentity(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	defer provider.Shutdown(context.Background())
	observer, err := observability.NewAsyncObserver(&observability.Runtime{Enabled: true, Provider: provider})
	require.NoError(t, err)
	runtime := &Runtime{observer: observer}
	const generation = "d62c3448-e786-4dc6-aee9-baed7398d4e3"
	_, finish := runtime.startBuildObservation(context.Background(), BuildClaim{GenerationID: generation})
	finish("published")
	require.Len(t, recorder.Ended(), 1)
	attrs := map[string]string{}
	for _, a := range recorder.Ended()[0].Attributes() {
		attrs[string(a.Key)] = a.Value.AsString()
	}
	require.Equal(t, generation, attrs["app.generation_id"], "generation must remain correlatable even when no source member existed at claim time")
}
