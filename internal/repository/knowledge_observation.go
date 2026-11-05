package repository

import (
	"context"
	"database/sql"
	"time"

	"video-support-agent/internal/eventbus"
	"video-support-agent/internal/knowledgeworker"
	"video-support-agent/internal/model"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var _ knowledgeworker.ObservationStore = (*KnowledgeBuildRepository)(nil)

func (r *KnowledgeBuildRepository) Observe(ctx context.Context) (knowledgeworker.Observation, error) {
	var snapshot knowledgeworker.Observation

	if r == nil || r.db == nil {
		return snapshot, knowledgeworker.ErrObservation
	}

	bounded, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	db := r.db.Session(&gorm.Session{Logger: logger.Default.LogMode(logger.Silent)}).WithContext(bounded)

	err := db.Transaction(func(tx *gorm.DB) error {
		var control model.KnowledgeIndexControl
		if err := tx.
			Select("desired_epoch", "active_epoch").
			Where("id = ?", model.KnowledgeIndexControlID).
			Take(&control).Error; err != nil {
			return err
		}
		if control.DesiredEpoch == 0 || control.ActiveEpoch > control.DesiredEpoch {
			return knowledgeworker.ErrObservation
		}

		now, err := buildDatabaseTime(tx)
		if err != nil {
			return err
		}

		snapshot.At = now
		snapshot.DesiredEpoch = control.DesiredEpoch
		snapshot.ActiveEpoch = control.ActiveEpoch

		if err := observeStateBuckets(tx, &snapshot); err != nil {
			return err
		}
		if err := observeDueAttempts(tx, &snapshot); err != nil {
			return err
		}
		return observeAttemptLinks(tx, &snapshot)
	}, &sql.TxOptions{
		Isolation: sql.LevelRepeatableRead,
		ReadOnly:  true,
	})

	if ctx.Err() != nil {
		return knowledgeworker.Observation{}, ctx.Err()
	}
	if err != nil || bounded.Err() != nil {
		return knowledgeworker.Observation{}, knowledgeworker.ErrObservation
	}
	return snapshot, nil
}

func observeStateBuckets(tx *gorm.DB, snapshot *knowledgeworker.Observation) error {
	groups := []struct {
		value  any
		scope  string
		states []string
	}{
		{
			&model.KnowledgeEventInbox{}, "inbox",
			[]string{
				model.KnowledgeEventReceived,
				model.KnowledgeEventWaiting,
				model.KnowledgeEventApplied,
				model.KnowledgeEventIgnoredDraft,
				model.KnowledgeEventSuperseded,
				model.KnowledgeEventDeadLettered,
			},
		},
		{
			&model.KnowledgeIndexAttempt{}, "attempt",
			[]string{
				model.KnowledgeAttemptScheduled,
				model.KnowledgeAttemptRunning,
				model.KnowledgeAttemptSucceeded,
				model.KnowledgeAttemptFailed,
				model.KnowledgeAttemptObsolete,
			},
		},
		{
			&model.KnowledgeIndexTask{}, "task",
			[]string{
				model.KnowledgeTaskPending,
				model.KnowledgeTaskBuilding,
				model.KnowledgeTaskRetryWait,
				model.KnowledgeTaskCompleted,
				model.KnowledgeTaskObsolete,
				model.KnowledgeTaskBlocked,
			},
		},
		{
			&model.KnowledgeBuildRecovery{}, "recovery",
			[]string{
				model.KnowledgeRecoveryPending,
				model.KnowledgeRecoveryCompleted,
			},
		},
	}

	for _, group := range groups {
		if err := appendObservationBuckets(
			tx, snapshot, group.value,
			group.scope, "", group.states,
		); err != nil {
			return err
		}
	}

	for _, topic := range eventbus.Topics() {
		if err := appendObservationBuckets(
			tx, snapshot, &model.EventOutbox{}, "outbox", topic,
			[]string{
				model.OutboxPending,
				model.OutboxSending,
				model.OutboxSent,
				model.OutboxBlocked,
			},
		); err != nil {
			return err
		}
	}

	return tx.Model(&model.KnowledgePoison{}).Count(&snapshot.PoisonCount).Error
}

func appendObservationBuckets(tx *gorm.DB, snapshot *knowledgeworker.Observation, value any, scope, topic string, states []string) error {
	var rows []knowledgeworker.StateBucket

	query := tx.Model(value).
		Where("status IN ?", states)
	if topic != "" {
		query = query.Where("topic = ?", topic)
	}

	if err := query.Select(
		"status AS state, COUNT(*) AS count, "+
			"TIMESTAMPDIFF(MICROSECOND, MIN(created_at), ?) "+
			"/ 1000000.0 AS oldest_age_seconds",
		snapshot.At,
	).Group("status").Scan(&rows).Error; err != nil {
		return err
	}

	found := make(map[string]knowledgeworker.StateBucket, len(rows))
	for _, row := range rows {
		found[row.State] = row
	}

	for _, state := range states {
		row := found[state]
		row.Scope = scope
		row.Topic = topic
		row.State = state
		snapshot.Buckets = append(snapshot.Buckets, row)
	}
	return nil
}

func observeDueAttempts(tx *gorm.DB, snapshot *knowledgeworker.Observation) error {
	var row struct {
		Seconds *float64
	}

	if err := tx.Model(&model.KnowledgeIndexAttempt{}).
		Where(
			"status = ? AND not_before <= ?",
			model.KnowledgeAttemptScheduled,
			snapshot.At,
		).
		Select(
			"TIMESTAMPDIFF(MICROSECOND, MIN(not_before), ?) "+
				"/ 1000000.0 AS seconds",
			snapshot.At,
		).
		Scan(&row).Error; err != nil {
		return err
	}

	snapshot.OldestDueSeconds = row.Seconds
	return nil
}

func observeAttemptLinks(tx *gorm.DB, snapshot *knowledgeworker.Observation) error {
	states := []string{
		model.KnowledgeAttemptScheduled,
		model.KnowledgeAttemptRunning,
		model.KnowledgeAttemptSucceeded,
		model.KnowledgeAttemptFailed,
		model.KnowledgeAttemptObsolete,
	}

	if err := tx.Model(&model.KnowledgeIndexAttempt{}).
		Select(
			"event_id, attempt AS event_attempt, generation_id, "+
				"status, not_before, started_at, completed_at, updated_at",
		).
		Where("status IN ?", states).
		Order("updated_at DESC").
		Order("event_id").
		Order("attempt").
		Limit(32).
		Scan(&snapshot.Attempts).Error; err != nil {
		return err
	}

	if len(snapshot.Attempts) == 0 {
		return nil
	}

	ids := make([]string, 0, len(snapshot.Attempts))
	for _, row := range snapshot.Attempts {
		ids = append(ids, row.EventID)
	}

	var inboxes []struct {
		EventID string
		TraceID string
	}
	if err := tx.Model(&model.KnowledgeEventInbox{}).
		Select("event_id", "trace_id").
		Where("event_id IN ?", ids).
		Scan(&inboxes).Error; err != nil {
		return err
	}

	traces := make(map[string]string, len(inboxes))
	for _, inbox := range inboxes {
		traces[inbox.EventID] = inbox.TraceID
	}
	for i := range snapshot.Attempts {
		snapshot.Attempts[i].TraceID = traces[snapshot.Attempts[i].EventID]
	}
	return nil
}
