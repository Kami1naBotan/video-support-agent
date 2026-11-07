package observability

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func contractRuntime(t *testing.T) (*Runtime, *tracetest.SpanRecorder) {
	t.Helper()
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()), sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { require.NoError(t, provider.Shutdown(context.Background())) })
	return &Runtime{Enabled: true, Registry: prometheus.NewRegistry(), Provider: provider, Propagator: propagation.TraceContext{}}, recorder
}

func TestObservationQueryContract(t *testing.T) {
	for _, backend := range []string{"elasticsearch", "milvus"} {
		t.Run(backend, func(t *testing.T) {
			rt, rec := contractRuntime(t)
			observer, err := NewQueryObserver(rt, backend)
			require.NoError(t, err)
			_, finish := observer.Start(context.Background())
			var wg sync.WaitGroup
			for range 8 {
				wg.Add(1)
				go func() { defer wg.Done(); finish(context.DeadlineExceeded) }()
			}
			wg.Wait()
			require.Len(t, rec.Ended(), 1)
			families, err := rt.Registry.Gather()
			require.NoError(t, err)
			require.Len(t, families, 1)
			require.EqualValues(t, 1, families[0].Metric[0].Histogram.GetSampleCount())
			require.Equal(t, "timed_out", families[0].Metric[0].Label[0].GetValue())
		})
	}
}

func TestObservationLLMBodyContract(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		consume bool
		want    string
	}{
		{"eof", 200, true, "success"}, {"early_close", 200, false, "closed"}, {"unauthorized", 401, true, "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt, rec := contractRuntime(t)
			observer, err := NewLLMObserver(rt)
			require.NoError(t, err)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Empty(t, r.Header.Get("traceparent"))
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, "synthetic-private-response")
			}))
			defer server.Close()
			client, err := observer.HTTPClient("planner", time.Second)
			require.NoError(t, err)
			request, err := http.NewRequest("POST", server.URL+"/synthetic-private-path", strings.NewReader("synthetic-private-prompt"))
			require.NoError(t, err)
			request.Header.Set("Authorization", "Bearer synthetic-private-key")
			response, err := client.Do(request)
			require.NoError(t, err)
			require.Empty(t, rec.Ended(), "headers alone must not finish the span")
			if tc.consume {
				_, err = io.Copy(io.Discard, response.Body)
				require.NoError(t, err)
			}
			require.NoError(t, response.Body.Close())
			require.NoError(t, response.Body.Close())
			require.Len(t, rec.Ended(), 1)
			families, err := rt.Registry.Gather()
			require.NoError(t, err)
			require.Len(t, families, 1)
			metric := families[0].Metric[0]
			require.EqualValues(t, 1, metric.Histogram.GetSampleCount())
			values := map[string]string{}
			for _, label := range metric.Label {
				values[label.GetName()] = label.GetValue()
			}
			require.Equal(t, tc.want, values["outcome"])
			require.NotContains(t, fmt.Sprint(rec.Ended()[0].Attributes()), "synthetic-private")
		})
	}
}

func TestObservationLLMCancellation(t *testing.T) {
	rt, rec := contractRuntime(t)
	observer, err := NewLLMObserver(rt)
	require.NoError(t, err)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	client, err := observer.HTTPClient("answer", time.Second)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, "GET", server.URL, nil)
	response, err := client.Do(request)
	require.NoError(t, err)
	cancel()
	require.Eventually(t, func() bool { return len(rec.Ended()) == 1 }, time.Second, time.Millisecond)
	_ = response.Body.Close()
	require.Len(t, rec.Ended(), 1)
	require.Contains(t, fmt.Sprint(rec.Ended()[0].Attributes()), "cancelled")
}

func TestObservationAsyncLinks(t *testing.T) {
	rt, rec := contractRuntime(t)
	observer, err := NewAsyncObserver(rt)
	require.NoError(t, err)
	ctx, parent := rt.Provider.Tracer("test").Start(context.Background(), "source")
	carrier := CaptureTraceParent(ctx)
	parent.End()
	receive, finish := observer.Start(context.Background(), "receive", carrier, nil)
	require.Equal(t, trace.SpanContextFromContext(ctx).TraceID(), trace.SpanContextFromContext(receive).TraceID())
	finish("success")
	build, end := observer.Start(context.Background(), "build", "", []string{carrier, carrier, "secret-invalid"})
	end("published")
	require.NotEqual(t, trace.SpanContextFromContext(ctx).TraceID(), trace.SpanContextFromContext(build).TraceID())
	spans := rec.Ended()
	require.Len(t, spans, 3)
	require.Len(t, spans[2].Links(), 1)
	require.Equal(t, trace.SpanContextFromContext(ctx).SpanID(), spans[2].Links()[0].SpanContext.SpanID())
	require.Empty(t, NormalizeTraceParent("secret-invalid"))
}

func TestObservationExecutionCounters(t *testing.T) {
	rt, rec := contractRuntime(t)
	observer, err := NewExecutionObserver(rt)
	require.NoError(t, err)
	for _, outcome := range []string{"completed", "cancelled", "failed", "timed_out", "unknown"} {
		_, finish := observer.Start(context.Background(), "agent", "chat")
		result := ExecutionResult{Outcome: outcome, Reason: outcome, BusinessEffect: "committed"}
		finish(result)
		finish(result)
	}
	_, finish := observer.Start(context.Background(), "search_rule", "chat")
	finish(ExecutionResult{Outcome: "failed", Reason: "synthetic-private-error"})
	require.Len(t, rec.Ended(), 6)
	families, err := rt.Registry.Gather()
	require.NoError(t, err)
	totals := map[string]float64{}
	for _, f := range families {
		for _, m := range f.Metric {
			totals[f.GetName()] += m.GetCounter().GetValue()
			require.NotContains(t, m.String(), "synthetic-private")
		}
	}
	require.Equal(t, float64(3), totals["agent_failures_total"])
	require.Equal(t, float64(1), totals["agent_cancellations_total"])
	require.Equal(t, float64(1), totals["tool_calls_total"])
	require.Equal(t, "failed", queryOutcome(errors.New("synthetic-private-error")))
}
