package observability

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/stretchr/testify/require"
	"video-support-agent/internal/config"
)

func TestObservationRuntimeLifecycle(t *testing.T) {
	disabled, err := New(context.Background(), config.ObservabilityConfig{Enabled: "false"}, "api")
	require.NoError(t, err)
	server, err := StartMetrics(disabled)
	require.NoError(t, err)
	require.Nil(t, server.Errors())
	require.NoError(t, server.Shutdown(context.Background()))
	require.NoError(t, disabled.Shutdown(context.Background()))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = New(ctx, config.ObservabilityConfig{Enabled: "false"}, "api")
	require.ErrorIs(t, err, context.Canceled)
	var requests atomic.Int32
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/traces" {
			w.WriteHeader(404)
			return
		}
		requests.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(200)
	}))
	defer collector.Close()
	cfg := config.ObservabilityConfig{Enabled: "true", ServiceName: "contract", OTLPEndpoint: collector.URL, SampleRatio: "1", MetricsAddr: "127.0.0.1:9464"}
	runtime, err := New(context.Background(), cfg, "worker")
	require.NoError(t, err)
	_, span := runtime.Provider.Tracer("test").Start(context.Background(), "shutdown-flush")
	span.End()
	require.NoError(t, runtime.Shutdown(context.Background()))
	require.Greater(t, requests.Load(), int32(0))
	count := requests.Load()
	require.NoError(t, runtime.Shutdown(context.Background()))
	require.Equal(t, count, requests.Load())
}

func TestObservationMetricsLifecycle(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	address := listener.Addr().String()
	runtime := &Runtime{Enabled: true, Registry: prometheus.NewRegistry(), MetricsAddr: address}
	_, err = StartMetrics(runtime)
	require.Error(t, err)
	require.NoError(t, listener.Close())
	server, err := StartMetrics(runtime)
	require.NoError(t, err)
	defer server.Shutdown(context.Background())
	client := &http.Client{Timeout: time.Second}
	for _, tc := range []struct {
		method, path string
		status       int
	}{{"GET", "/metrics", 200}, {"GET", "/not-metrics", 404}, {"POST", "/metrics", 405}} {
		request, _ := http.NewRequest(tc.method, "http://"+address+tc.path, nil)
		response, err := client.Do(request)
		require.NoError(t, err)
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		require.Equal(t, tc.status, response.StatusCode)
	}
	require.NoError(t, server.Shutdown(context.Background()))
	require.NoError(t, server.Shutdown(context.Background()))
	response, err := client.Get("http://" + address + "/metrics")
	if response != nil {
		_ = response.Body.Close()
	}
	require.Error(t, err)
}

func TestObservationLLMTimeoutAndRedirect(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		rt, rec := contractRuntime(t)
		observer, err := NewLLMObserver(rt)
		require.NoError(t, err)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
		defer server.Close()
		client, err := observer.HTTPClient("planner", 100*time.Millisecond)
		require.NoError(t, err)
		_, err = client.Get(server.URL)
		require.Error(t, err)
		require.Eventually(t, func() bool { return len(rec.Ended()) == 1 }, time.Second, time.Millisecond)
		require.Contains(t, fmt.Sprint(rec.Ended()[0].Attributes()), "timed_out")
	})
	t.Run("redirect", func(t *testing.T) {
		rt, rec := contractRuntime(t)
		observer, err := NewLLMObserver(rt)
		require.NoError(t, err)
		var hits atomic.Int32
		destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
		defer destination.Close()
		source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, destination.URL, 302) }))
		defer source.Close()
		client, err := observer.HTTPClient("answer", time.Second)
		require.NoError(t, err)
		request, _ := http.NewRequest("GET", source.URL, nil)
		request.Header.Set("Authorization", "Bearer synthetic-private-key")
		response, err := client.Do(request)
		require.NoError(t, err)
		require.Equal(t, 302, response.StatusCode)
		_, _ = io.Copy(io.Discard, response.Body)
		require.NoError(t, response.Body.Close())
		require.Zero(t, hits.Load())
		require.Len(t, rec.Ended(), 1)
	})
}

func TestObservationCollectorFailureIsSafe(t *testing.T) {
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		_, _ = io.WriteString(w, "synthetic-private-collector-error")
	}))
	defer collector.Close()
	cfg := config.ObservabilityConfig{Enabled: "true", ServiceName: "contract", OTLPEndpoint: collector.URL, SampleRatio: "1", MetricsAddr: "127.0.0.1:9464"}
	runtime, err := New(context.Background(), cfg, "api")
	require.NoError(t, err)
	_, span := runtime.Provider.Tracer("test").Start(context.Background(), "business-completed")
	span.End()
	started := time.Now()
	err = runtime.Shutdown(context.Background())
	require.Less(t, time.Since(started), 6*time.Second)
	if err != nil {
		require.NotContains(t, err.Error(), "synthetic-private")
	}
	recorder := httptest.NewRecorder()
	promhttp.HandlerFor(runtime.Registry, promhttp.HandlerOpts{}).ServeHTTP(recorder, httptest.NewRequest("GET", "/metrics", nil))
	require.Equal(t, 200, recorder.Code)
	require.NotContains(t, recorder.Body.String(), "synthetic-private")
}

// Model the standard library's legacy Request.Cancel timer winning just before
// the context deadline callback runs: deadline elapsed, Err not populated yet.
type delayedDeadlineContext struct{ context.Context }

func (delayedDeadlineContext) Deadline() (time.Time, bool) { return time.Now().Add(-time.Second), true }

type timeoutRaceTransport struct{}

func (timeoutRaceTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("synthetic transport cancellation")
}
func TestObservationLLMDeadlineRace(t *testing.T) {
	rt, rec := contractRuntime(t)
	observer, err := NewLLMObserver(rt)
	require.NoError(t, err)
	transport := &llmTransport{base: timeoutRaceTransport{}, observer: observer, role: "planner"}
	request, _ := http.NewRequestWithContext(delayedDeadlineContext{context.Background()}, "GET", "http://example.invalid", nil)
	_, err = transport.RoundTrip(request)
	require.Error(t, err)
	require.Len(t, rec.Ended(), 1)
	require.Contains(t, fmt.Sprint(rec.Ended()[0].Attributes()), "timed_out")
}
