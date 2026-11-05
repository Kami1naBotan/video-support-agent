package knowledgeworker

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"video-support-agent/internal/eventbus"

	"go.uber.org/zap"
)

var ErrObservation = errors.New("knowledge observation unavailable")

type StateBucket struct {
	Scope            string   `json:"scope"`
	Topic            string   `json:"topic,omitempty"`
	State            string   `json:"state"`
	Count            int64    `json:"count"`
	OldestAgeSeconds *float64 `json:"oldest_created_age_seconds"`
}

type AttemptObservation struct {
	EventID      string
	TraceID      string
	EventAttempt uint32
	GenerationID *string
	Status       string
	NotBefore    time.Time
	StartedAt    *time.Time
	CompletedAt  *time.Time
	UpdatedAt    time.Time
}

type Observation struct {
	At               time.Time
	DesiredEpoch     uint64
	ActiveEpoch      uint64
	Buckets          []StateBucket
	PoisonCount      int64
	OldestDueSeconds *float64
	Attempts         []AttemptObservation
}

type ObservationStore interface {
	Observe(context.Context) (Observation, error)
}

type LagReader interface {
	SampleLag(context.Context, string, string) ([]eventbus.PartitionLag, error)
}

type LagTarget struct {
	Group string
	Topic string
}

func RunObservations(ctx context.Context, store ObservationStore, broker LagReader, log *zap.Logger, targets []LagTarget) error {
	if store == nil || broker == nil || log == nil || len(targets) != 2 {
		return ErrBuildConfig
	}

	seen := make(map[string]bool, 2)
	for _, target := range targets {
		if target.Group == "" || (target.Topic != eventbus.TopicDocumentEvents && target.Topic != eventbus.TopicIndexRetry) || seen[target.Topic] {
			return ErrBuildConfig
		}
		seen[target.Topic] = true
	}
	targets = append([]LagTarget(nil), targets...)

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		snapshot, err := store.Observe(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			log.Warn("knowledge observation",
				zap.String("scope", "database"),
				zap.Bool("available", false),
				zap.String("code", "database_sample_unavailable"))
		} else {
			log.Info("knowledge observation",
				zap.String("scope", "database"),
				zap.Bool("available", true),
				zap.Time("sample_at", snapshot.At),
				zap.Uint64("desired_epoch", snapshot.DesiredEpoch),
				zap.Uint64("active_epoch", snapshot.ActiveEpoch),
				zap.Uint64("epoch_gap", snapshot.DesiredEpoch-snapshot.ActiveEpoch),
				zap.Any("state_counts", snapshot.Buckets),
				zap.Int64("poison_count", snapshot.PoisonCount),
				zap.Any("oldest_due_wait_seconds", snapshot.OldestDueSeconds))

			for _, attempt := range snapshot.Attempts {
				logAttemptObservation(log, snapshot.At, attempt)
			}
		}

		for _, target := range targets {
			rows, err := broker.SampleLag(ctx, target.Group, target.Topic)
			if ctx.Err() != nil {
				return ctx.Err()
			}

			fields := []zap.Field{
				zap.String("scope", "kafka"),
				zap.String("topic", target.Topic),
				zap.Bool("available", err == nil),
			}
			if err != nil {
				log.Warn("knowledge observation", append(fields, zap.String("code", "lag_sample_unavailable"))...)
				continue
			}

			log.Info("knowledge observation", append(fields, zap.Any("partitions", rows))...)
		}

		if err := runtimeWait(ctx, 15*time.Second); err != nil {
			return err
		}
	}
}

var observationIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

var observationTracePattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

func logAttemptObservation(log *zap.Logger, at time.Time, row AttemptObservation) {
	if !observationIDPattern.MatchString(row.EventID) ||
		!observationTracePattern.MatchString(row.TraceID) ||
		row.TraceID == strings.Repeat("0", 32) ||
		(row.GenerationID != nil && !observationIDPattern.MatchString(*row.GenerationID)) {
		log.Warn("attempt observation unavailable", zap.String("code", "correlation_invalid"))
		return
	}

	fields := []zap.Field{
		zap.Time("sample_at", at),
		zap.String("event_id", row.EventID),
		zap.String("trace_id", row.TraceID),
		zap.Uint32("event_attempt", row.EventAttempt),
		zap.String("state", row.Status),
		zap.Time("attempt_updated_at", row.UpdatedAt),
	}

	if row.GenerationID != nil {
		fields = append(fields,
			zap.String("generation_id", *row.GenerationID))
	}

	if row.StartedAt != nil {
		delay := row.StartedAt.Sub(row.NotBefore).Seconds()
		fields = append(fields,
			zap.Float64("schedule_delay_seconds", delay),
			zap.Bool("schedule_time_inconsistent", delay < 0))
	}

	if row.StartedAt != nil && row.CompletedAt != nil {
		elapsed := row.CompletedAt.Sub(*row.StartedAt).Seconds()
		fields = append(fields,
			zap.Float64("attempt_elapsed_seconds", elapsed),
			zap.Bool("attempt_time_inconsistent", elapsed < 0))
	}

	log.Info("knowledge attempt sample", fields...)
}
