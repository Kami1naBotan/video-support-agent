//go:build observationbackend

package observability

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"
	"video-support-agent/internal/config"
)

// Verifies real backend transport; the LLM response is deliberately synthetic.
// This does not replace the vendor/retrieval or full API/worker acceptance.
func TestObservationRealBackends(t *testing.T) {
	require.Equal(t, "1", os.Getenv("OBS_BACKEND_TEST_ISOLATED"))
	client := &http.Client{Timeout: 3 * time.Second}
	get := func(address string) ([]byte, error) {
		r, err := client.Get(address)
		if err != nil {
			return nil, err
		}
		defer r.Body.Close()
		if r.StatusCode != 200 {
			return nil, fmt.Errorf("backend status %d", r.StatusCode)
		}
		return io.ReadAll(io.LimitReader(r.Body, 4<<20))
	}
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "synthetic-body-private-canary") }))
	defer model.Close()
	for index, processRole := range []string{"api", "worker"} {
		rt, err := New(context.Background(), config.ObservabilityConfig{Enabled: "true", ServiceName: "backend-acceptance", SampleRatio: "1", OTLPEndpoint: "http://127.0.0.1:4318", MetricsAddr: fmt.Sprintf("127.0.0.1:%d", 9464+index)}, processRole)
		require.NoError(t, err)
		defer rt.Shutdown(context.Background())
		metrics, err := StartMetrics(rt)
		require.NoError(t, err)
		defer metrics.Shutdown(context.Background())
		observer, err := NewLLMObserver(rt)
		require.NoError(t, err)
		ctx, root := rt.Provider.Tracer("acceptance").Start(context.Background(), "backend-transport")
		id := trace.SpanContextFromContext(ctx).TraceID().String()
		for _, role := range []string{"planner", "answer"} {
			llm, err := observer.HTTPClient(role, time.Second)
			require.NoError(t, err)
			req, err := http.NewRequestWithContext(ctx, "GET", model.URL, nil)
			require.NoError(t, err)
			response, err := llm.Do(req)
			require.NoError(t, err)
			_, err = io.Copy(io.Discard, response.Body)
			require.NoError(t, err)
			require.NoError(t, response.Body.Close())
		}
		root.End()
		if processRole == "worker" {
			async, err := NewAsyncObserver(rt)
			require.NoError(t, err)
			publish, finishPublish := async.Start(context.Background(), "publish", CaptureTraceParent(ctx), nil)
			finishPublish("success")
			received, finishReceive := async.Start(context.Background(), "receive", CaptureTraceParent(publish), nil)
			finishReceive("success")
			build, finishBuild := async.Start(context.Background(), "build", "", []string{CaptureTraceParent(received)})
			finishBuild("published")
			buildID := trace.SpanContextFromContext(build).TraceID().String()
			require.NotEqual(t, id, buildID)
			require.NoError(t, rt.provider.ForceFlush(context.Background()))
			require.Eventually(t, func() bool {
				body, err := get("http://127.0.0.1:16686/api/traces/" + buildID)
				if err != nil {
					return false
				}
				var result struct {
					Data []struct {
						Spans []struct {
							References []struct {
								TraceID string `json:"traceID"`
								SpanID  string `json:"spanID"`
							} `json:"references"`
						} `json:"spans"`
					} `json:"data"`
				}
				if json.Unmarshal(body, &result) != nil || len(result.Data) != 1 || len(result.Data[0].Spans) != 1 {
					return false
				}
				for _, ref := range result.Data[0].Spans[0].References {
					if ref.TraceID == id && ref.SpanID == trace.SpanContextFromContext(received).SpanID().String() {
						return true
					}
				}
				return false
			}, 15*time.Second, 250*time.Millisecond)
			t.Logf("build trace=%s links source trace=%s", buildID, id)
		}
		require.NoError(t, rt.provider.ForceFlush(context.Background()))
		require.Eventually(t, func() bool {
			body, err := get("http://127.0.0.1:16686/api/traces/" + id)
			if err != nil {
				return false
			}
			require.NotContains(t, string(body), "synthetic-body-private-canary")
			var result struct {
				Data []struct {
					Spans []json.RawMessage `json:"spans"`
				} `json:"data"`
			}
			expected := 3
			if processRole == "worker" {
				expected = 5
			}
			return json.Unmarshal(body, &result) == nil && len(result.Data) == 1 && len(result.Data[0].Spans) == expected
		}, 15*time.Second, 250*time.Millisecond)
		for _, role := range []string{"planner", "answer"} {
			query := fmt.Sprintf(`llm_call_duration_seconds_count{job="video-support-agent-%s",process_role="%s",role="%s",outcome="success"}`, processRole, processRole, role)
			require.Eventually(t, func() bool {
				body, err := get("http://127.0.0.1:9090/api/v1/query?query=" + url.QueryEscape(query))
				if err != nil {
					return false
				}
				var result struct {
					Status string `json:"status"`
					Data   struct {
						Result []struct {
							Value []json.RawMessage `json:"value"`
						} `json:"result"`
					} `json:"data"`
				}
				if json.Unmarshal(body, &result) != nil || result.Status != "success" || len(result.Data.Result) != 1 {
					return false
				}
				values := result.Data.Result[0].Value
				return len(values) == 2 && string(values[1]) == `"1"`
			}, 40*time.Second, time.Second)
		}
		t.Logf("process=%s trace=%s: Jaeger has parent plus two LLM spans; Prometheus preserves both roles", processRole, id)
	}
}

func TestObservationJaegerExport(t *testing.T) {
	require.Equal(t, "1", os.Getenv("OBS_BACKEND_TEST_ISOLATED"))
	rt, err := New(context.Background(), config.ObservabilityConfig{Enabled: "true", ServiceName: "jaeger-acceptance", SampleRatio: "1", OTLPEndpoint: "http://127.0.0.1:4318", MetricsAddr: "127.0.0.1:9465"}, "worker")
	require.NoError(t, err)
	defer rt.Shutdown(context.Background())
	async, err := NewAsyncObserver(rt)
	require.NoError(t, err)
	ctx, root := rt.Provider.Tracer("acceptance").Start(context.Background(), "source")
	root.End()
	received, finish := async.Start(context.Background(), "receive", CaptureTraceParent(ctx), nil)
	finish("success")
	build, finish := async.Start(context.Background(), "build", "", []string{CaptureTraceParent(received)})
	finish("published")
	require.NoError(t, rt.provider.ForceFlush(context.Background()))
	sourceID := trace.SpanContextFromContext(ctx).TraceID().String()
	buildID := trace.SpanContextFromContext(build).TraceID().String()
	require.NotEqual(t, sourceID, buildID)
	client := &http.Client{Timeout: 3 * time.Second}
	for _, id := range []string{sourceID, buildID} {
		require.Eventually(t, func() bool {
			resp, err := client.Get("http://127.0.0.1:16686/api/traces/" + id)
			if err != nil {
				return false
			}
			defer resp.Body.Close()
			var result struct {
				Data []struct {
					Spans []struct {
						References []struct {
							TraceID string `json:"traceID"`
							SpanID  string `json:"spanID"`
						} `json:"references"`
					} `json:"spans"`
				} `json:"data"`
			}
			if json.NewDecoder(resp.Body).Decode(&result) != nil || len(result.Data) != 1 {
				return false
			}
			if id == sourceID {
				return len(result.Data[0].Spans) == 2
			}
			if len(result.Data[0].Spans) != 1 {
				return false
			}
			for _, ref := range result.Data[0].Spans[0].References {
				if ref.TraceID == sourceID && ref.SpanID == trace.SpanContextFromContext(received).SpanID().String() {
					return true
				}
			}
			return false
		}, 15*time.Second, 250*time.Millisecond)
	}
	t.Logf("source_trace=%s build_trace=%s: real OTLP export, new root and cross-trace link verified", sourceID, buildID)
}

func TestObservationBackendRecovery(t *testing.T) {
	require.Equal(t, "1", os.Getenv("OBS_BACKEND_TEST_ISOLATED"))
	project := os.Getenv("OBS_BACKEND_TEST_PROJECT")
	require.True(t, strings.HasPrefix(project, "observation-test-"))
	container := project + "-jaeger-1"
	command := func(args ...string) []byte {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		require.NoError(t, err, "docker operation failed")
		return out
	}
	owner := command("inspect", "--format", `{{index .Config.Labels "com.docker.compose.project"}}`, container)
	require.Equal(t, project, strings.TrimSpace(string(owner)))
	rt, err := New(context.Background(), config.ObservabilityConfig{Enabled: "true", ServiceName: "recovery-test", SampleRatio: "1", OTLPEndpoint: "http://127.0.0.1:4318", MetricsAddr: "127.0.0.1:9464"}, "api")
	require.NoError(t, err)
	defer rt.Shutdown(context.Background())
	command("stop", container)
	defer command("start", container)
	started := time.Now()
	_, span := rt.Provider.Tracer("acceptance").Start(context.Background(), "during-outage")
	span.End()
	require.Error(t, rt.provider.ForceFlush(context.Background()))
	require.Less(t, time.Since(started), 6*time.Second)
	command("start", container)
	client := &http.Client{Timeout: time.Second}
	require.Eventually(t, func() bool {
		r, e := client.Get("http://127.0.0.1:16686/api/services")
		if e != nil {
			return false
		}
		r.Body.Close()
		return r.StatusCode == 200
	}, 20*time.Second, 250*time.Millisecond)
	ctx, span := rt.Provider.Tracer("acceptance").Start(context.Background(), "after-recovery")
	id := trace.SpanContextFromContext(ctx).TraceID().String()
	span.End()
	require.NoError(t, rt.provider.ForceFlush(context.Background()))
	require.Eventually(t, func() bool {
		r, e := client.Get("http://127.0.0.1:16686/api/traces/" + id)
		if e != nil {
			return false
		}
		defer r.Body.Close()
		var result struct {
			Data []json.RawMessage `json:"data"`
		}
		return json.NewDecoder(r.Body).Decode(&result) == nil && len(result.Data) == 1
	}, 10*time.Second, 250*time.Millisecond)
	t.Log("collector outage returns bounded safe error; same Runtime exports new spans after collector restart; dropped outage spans are not promised replay")
}
