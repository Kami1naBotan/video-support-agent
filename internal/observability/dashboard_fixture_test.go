//go:build observationbackend

package observability

import (
	"context"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	"os"
	"testing"
	"time"
)

// Synthetic rendering fixture only: these values are not performance evidence
// and do not prove application instrumentation. Uses disposable backends.
func TestObservationDashboardFixture(t *testing.T) {
	require.Equal(t, "1", os.Getenv("OBS_BACKEND_TEST_ISOLATED"))
	api := &Runtime{Enabled: true, MetricsAddr: "127.0.0.1:9464", Registry: prometheus.NewRegistry()}
	worker := &Runtime{Enabled: true, MetricsAddr: "127.0.0.1:9465", Registry: prometheus.NewRegistry()}
	for _, rt := range []*Runtime{api, worker} {
		s, e := StartMetrics(rt)
		require.NoError(t, e)
		defer s.Shutdown(context.Background())
	}
	hist := []prometheus.Observer{}
	for _, spec := range []struct {
		name   string
		labels prometheus.Labels
	}{
		{"http_server_request_duration_seconds", prometheus.Labels{"route": "/synthetic-fixture", "method": "GET", "status_class": "2xx"}},
		{"llm_call_duration_seconds", prometheus.Labels{"role": "planner", "outcome": "success"}},
		{"es_query_duration_seconds", prometheus.Labels{"outcome": "success"}},
		{"milvus_query_duration_seconds", prometheus.Labels{"outcome": "success"}},
	} {
		h := prometheus.NewHistogram(prometheus.HistogramOpts{Name: spec.name, Help: "Synthetic visualization fixture", ConstLabels: spec.labels})
		require.NoError(t, api.Registry.Register(h))
		hist = append(hist, h)
	}
	counters := []prometheus.Counter{}
	for _, spec := range []struct {
		name   string
		labels prometheus.Labels
	}{
		{"tool_calls_total", prometheus.Labels{"tool": "search_rule", "phase": "chat", "outcome": "success"}},
		{"agent_failures_total", prometheus.Labels{"phase": "chat", "reason": "failed"}},
		{"agent_cancellations_total", prometheus.Labels{"phase": "chat"}},
	} {
		c := prometheus.NewCounter(prometheus.CounterOpts{Name: spec.name, Help: "Synthetic visualization fixture", ConstLabels: spec.labels})
		require.NoError(t, api.Registry.Register(c))
		counters = append(counters, c)
	}
	gauges := []prometheus.Gauge{}
	for _, name := range []string{"kafka_consumer_lag", "kafka_consumer_lag_available", "kafka_consumer_lag_last_success_timestamp_seconds"} {
		g := prometheus.NewGauge(prometheus.GaugeOpts{Name: name, Help: "Synthetic visualization fixture", ConstLabels: prometheus.Labels{"group": "synthetic-fixture", "topic": "synthetic-fixture", "partition": "0"}})
		require.NoError(t, worker.Registry.Register(g))
		gauges = append(gauges, g)
	}
	for i := 0; i < 180; i++ {
		for _, h := range hist {
			h.Observe(0.05)
		}
		for _, c := range counters {
			c.Inc()
		}
		gauges[0].Set(3)
		gauges[1].Set(1)
		gauges[2].Set(float64(time.Now().Unix()))
		time.Sleep(time.Second)
	}
}
