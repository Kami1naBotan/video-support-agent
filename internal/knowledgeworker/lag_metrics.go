package knowledgeworker

import (
	"errors"
	"regexp"
	"strconv"
	"sync"
	"time"

	"video-support-agent/internal/eventbus"
	"video-support-agent/internal/observability"

	"github.com/prometheus/client_golang/prometheus"
)

const lagSampleMaxAge = 45 * time.Second

var lagGroupPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)

type lagPartitionState struct {
	Available   bool
	Lag         int64
	SampledAt   time.Time
	LastSuccess time.Time
}

type LagMetrics struct {
	mu      sync.RWMutex
	targets []LagTarget
	states  map[LagTarget][3]lagPartitionState

	lag         *prometheus.Desc
	available   *prometheus.Desc
	lastSuccess *prometheus.Desc
}

var _ prometheus.Collector = (*LagMetrics)(nil)

func NewLagMetrics(runtime *observability.Runtime, targets []LagTarget) (*LagMetrics, error) {
	if runtime == nil {
		return nil, errors.New("lag observation runtime is required")
	}
	if len(targets) != 2 {
		return nil, errors.New("invalid lag observation targets")
	}

	seen := make(map[string]bool, 2)
	for _, target := range targets {
		if !lagGroupPattern.MatchString(target.Group) ||
			(target.Topic != eventbus.TopicDocumentEvents && target.Topic != eventbus.TopicIndexRetry) ||
			seen[target.Topic] {
			return nil, errors.New("invalid lag observation targets")
		}
		seen[target.Topic] = true
	}

	if !runtime.Enabled {
		return nil, nil
	}
	if runtime.Registry == nil {
		return nil, errors.New("lag observation registry is required")
	}

	labels := []string{"group", "topic", "partition"}
	metrics := &LagMetrics{
		targets: append([]LagTarget(nil), targets...),
		states:  make(map[LagTarget][3]lagPartitionState, 2),
		lag: prometheus.NewDesc(
			"kafka_consumer_lag",
			"End offset minus committed offset for a valid fresh sample.",
			labels, nil,
		),
		available: prometheus.NewDesc(
			"kafka_consumer_lag_available",
			"Whether the partition has a valid fresh lag sample.",
			labels, nil,
		),
		lastSuccess: prometheus.NewDesc(
			"kafka_consumer_lag_last_success_timestamp_seconds",
			"Last valid partition lag sample time; zero means never.",
			labels, nil,
		),
	}

	for _, target := range metrics.targets {
		metrics.states[target] = [3]lagPartitionState{}
	}
	if err := runtime.Registry.Register(metrics); err != nil {
		return nil, errors.New("register lag metrics failed")
	}
	return metrics, nil
}

func (m *LagMetrics) Describe(ch chan<- *prometheus.Desc) {
	ch <- m.lag
	ch <- m.available
	ch <- m.lastSuccess
}

func (m *LagMetrics) Collect(ch chan<- prometheus.Metric) {
	m.mu.RLock()
	snapshots := make([][3]lagPartitionState, len(m.targets))
	for i, target := range m.targets {
		snapshots[i] = m.states[target]
	}
	m.mu.RUnlock()

	now := time.Now()
	for i, target := range m.targets {
		for partition, state := range snapshots[i] {
			labels := []string{target.Group, target.Topic, strconv.Itoa(partition)}

			age := now.Sub(state.SampledAt)
			available := state.Available && !state.SampledAt.IsZero() && age >= 0 && age <= lagSampleMaxAge

			flag := 0.0
			if available {
				flag = 1
				ch <- prometheus.MustNewConstMetric(m.lag, prometheus.GaugeValue, float64(state.Lag), labels...)
			}

			ch <- prometheus.MustNewConstMetric(m.available, prometheus.GaugeValue, flag, labels...)

			lastSuccess := 0.0
			if !state.LastSuccess.IsZero() {
				lastSuccess = float64(state.LastSuccess.UnixMilli()) / 1000
			}
			ch <- prometheus.MustNewConstMetric(m.lastSuccess, prometheus.GaugeValue, lastSuccess, labels...)
		}
	}
}

func (m *LagMetrics) Update(target LagTarget, rows []eventbus.PartitionLag, sampleErr error, sampledAt time.Time) {
	if m == nil {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	previous, exists := m.states[target]
	if !exists {
		return
	}

	var next [3]lagPartitionState
	for partition := range next {
		next[partition].LastSuccess = previous[partition].LastSuccess
	}

	if sampleErr != nil || len(rows) != 3 || sampledAt.IsZero() {
		m.states[target] = next
		return
	}

	var seen [3]bool
	for _, row := range rows {
		if row.Partition < 0 || row.Partition >= 3 || seen[row.Partition] {
			m.states[target] = next
			return
		}
		seen[row.Partition] = true
	}

	for _, row := range rows {
		if !validLagPartition(row) {
			continue
		}

		next[row.Partition] = lagPartitionState{
			Available:   true,
			Lag:         *row.Lag,
			SampledAt:   sampledAt,
			LastSuccess: sampledAt,
		}
	}

	m.states[target] = next
}

func validLagPartition(row eventbus.PartitionLag) bool {
	if row.Status != "available" ||
		row.Committed == nil ||
		row.Lag == nil ||
		row.Start < 0 ||
		row.End < row.Start {
		return false
	}

	committed := *row.Committed
	return committed >= row.Start && committed <= row.End && *row.Lag == row.End-committed
}

func (m *LagMetrics) Clear() {
	if m == nil {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	for target, states := range m.states {
		for partition := range states {
			states[partition].Available = false
			states[partition].Lag = 0
			states[partition].SampledAt = time.Time{}
		}
		m.states[target] = states
	}
}
