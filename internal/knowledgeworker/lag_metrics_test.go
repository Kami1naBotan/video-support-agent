package knowledgeworker

import (
	"errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	"sync"
	"testing"
	"time"
	"video-support-agent/internal/eventbus"
	"video-support-agent/internal/observability"
)

func TestObservationLagInvalidation(t *testing.T) {
	registry := prometheus.NewRegistry()
	targets := []LagTarget{{Group: "source-test", Topic: eventbus.TopicDocumentEvents}, {Group: "retry-test", Topic: eventbus.TopicIndexRetry}}
	metrics, err := NewLagMetrics(&observability.Runtime{Enabled: true, Registry: registry}, targets)
	require.NoError(t, err)
	committed, lag := int64(8), int64(2)
	rows := []eventbus.PartitionLag{{Partition: 0, Start: 0, End: 10, Committed: &committed, Lag: &lag, Status: "available"}, {Partition: 1, Status: "uncommitted"}, {Partition: 2, Status: "offset_out_of_range"}}
	count := func(name string) int {
		families, e := registry.Gather()
		require.NoError(t, e)
		for _, f := range families {
			if f.GetName() == name {
				return len(f.Metric)
			}
		}
		return 0
	}
	require.Zero(t, count("kafka_consumer_lag"))
	require.Equal(t, 6, count("kafka_consumer_lag_available"))
	metrics.Update(targets[0], rows, nil, time.Now())
	require.Equal(t, 1, count("kafka_consumer_lag"))
	metrics.Update(targets[0], nil, errors.New("unavailable"), time.Now())
	require.Zero(t, count("kafka_consumer_lag"))
	require.False(t, metrics.states[targets[0]][0].LastSuccess.IsZero())
	metrics.Update(targets[0], rows, nil, time.Now().Add(-time.Minute))
	require.Zero(t, count("kafka_consumer_lag"))
	metrics.Update(targets[0], rows, nil, time.Now())
	metrics.Clear()
	require.Zero(t, count("kafka_consumer_lag"))
	rows[2].Partition = 0
	metrics.Update(targets[0], rows, nil, time.Now())
	require.Zero(t, count("kafka_consumer_lag"))
}

func TestObservationLagConcurrentScrapes(t *testing.T) {
	registry := prometheus.NewRegistry()
	targets := []LagTarget{{Group: "source-test", Topic: eventbus.TopicDocumentEvents}, {Group: "retry-test", Topic: eventbus.TopicIndexRetry}}
	metrics, err := NewLagMetrics(&observability.Runtime{Enabled: true, Registry: registry}, targets)
	require.NoError(t, err)
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				metrics.Update(targets[0], nil, errors.New("unavailable"), time.Now())
				_, _ = registry.Gather()
				metrics.Clear()
			}
		}()
	}
	wg.Wait()
}
