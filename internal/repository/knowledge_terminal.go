package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"time"
	"video-support-agent/internal/observability"

	"video-support-agent/internal/eventbus"
	"video-support-agent/internal/knowledgeevent"
	"video-support-agent/internal/knowledgeworker"
	"video-support-agent/internal/model"

	"gorm.io/gorm"
)

const terminalBatchSize = 64

func terminalizeEvent(tx *gorm.DB, event model.KnowledgeEventInbox, outcome string, generationID *string, epoch uint64, now time.Time) error {
	messageID, err := knowledgeevent.NewID()
	if err != nil {
		return err
	}

	result := knowledgeevent.Result{
		SchemaVersion:       1,
		MessageID:           messageID,
		SourceEventID:       event.EventID,
		DocumentID:          strconv.FormatUint(event.DocumentID, 10),
		VersionID:           strconv.FormatUint(event.VersionID, 10),
		PublicationRevision: strconv.FormatUint(event.PublicationRevision, 10),
		Attempt:             event.ExpectedAttempt,
		Outcome:             outcome,
		GenerationID:        generationID,
		Epoch:               strconv.FormatUint(epoch, 10),
		OccurredAt:          now.UTC().Format(time.RFC3339Nano),
		TraceID:             event.TraceID,
	}
	payload, err := knowledgeevent.EncodeResult(result)
	if err != nil {
		return knowledgeworker.ErrBuildState
	}

	updated := tx.Model(&model.KnowledgeEventInbox{}).
		Where("event_id = ? AND terminal_message_id IS NULL AND expected_attempt = ?",
			event.EventID, event.ExpectedAttempt).
		Where("status IN ?", []string{
			model.KnowledgeEventReceived,
			model.KnowledgeEventWaiting,
		}).
		Updates(map[string]any{
			"status":              outcome,
			"terminal_message_id": messageID,
			"completed_at":        now,
			"safe_error_code":     "",
			"updated_at":          now,
		})
	if err := requireOnePublicationRow(updated); err != nil {
		return err
	}

	attemptStatus := model.KnowledgeAttemptSucceeded
	switch outcome {
	case knowledgeevent.ResultSuperseded:
		attemptStatus = model.KnowledgeAttemptObsolete
	case knowledgeevent.ResultDeadLettered:
		attemptStatus = model.KnowledgeAttemptFailed
	}

	updated = tx.Model(&model.KnowledgeIndexAttempt{}).
		Where("event_id = ? AND attempt = ? AND source_sha256 = ?",
			event.EventID, event.ExpectedAttempt, event.PayloadSHA256).
		Where("status IN ?", []string{
			model.KnowledgeAttemptScheduled,
			model.KnowledgeAttemptRunning,
		}).
		Where("not_before <= ?", now).
		Updates(map[string]any{
			"status":          attemptStatus,
			"generation_id":   generationID,
			"completed_at":    now,
			"safe_error_code": "",
			"updated_at":      now,
		})
	if err := requireOnePublicationRow(updated); err != nil {
		return err
	}

	hash := sha256.Sum256(payload)
	return tx.Create(&model.EventOutbox{
		MessageID:     messageID,
		DedupKey:      "terminal:" + event.EventID,
		EventID:       event.EventID,
		Topic:         eventbus.TopicIndexEvents,
		MessageKey:    result.DocumentID,
		TraceID:       event.TraceID,
		TraceParent:   observability.CaptureTraceParent(tx.Statement.Context, event.TraceParent),
		Payload:       payload,
		PayloadSHA256: hex.EncodeToString(hash[:]),
		Status:        model.OutboxPending,
		NextAttemptAt: now,
		CreatedAt:     now,
		UpdatedAt:     now,
	}).Error
}

func requireOnePublicationRow(result *gorm.DB) error {
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return knowledgeworker.ErrBuildState
	}
	return nil
}

func settleReadyEvents(tx *gorm.DB, control model.KnowledgeIndexControl, now time.Time) (int, error) {
	type readyEvent struct {
		model.KnowledgeEventInbox `gorm:"embedded"`
		CurrentRevision           uint64
	}

	var rows []readyEvent
	err := tx.Table("knowledge_event_inboxes AS i").
		Select("i.*, d.publication_revision AS current_revision").
		Joins("JOIN knowledge_documents AS d ON d.id = i.document_id").
		Joins(`JOIN knowledge_index_attempts AS a ON a.event_id = i.event_id AND a.attempt = i.expected_attempt`).
		Joins(`LEFT JOIN knowledge_document_indices AS s ON s.document_id = i.document_id`).
		Where("i.terminal_message_id IS NULL").
		Where(`NOT EXISTS (
				SELECT 1
				FROM knowledge_build_events AS b
				JOIN knowledge_build_recoveries AS r
					ON r.generation_id = b.generation_id
				WHERE b.event_id = i.event_id
					AND b.attempt = i.expected_attempt
					AND b.recovery_processed_at IS NULL
					AND r.status = ?
		)`, model.KnowledgeRecoveryPending).
		Where("i.status IN ?", []string{
			model.KnowledgeEventReceived,
			model.KnowledgeEventWaiting,
		}).
		Where("a.status IN ? AND a.not_before <= ?", []string{
			model.KnowledgeAttemptScheduled,
			model.KnowledgeAttemptRunning,
		}, now).
		Where(`(
			i.expected_attempt = 1
			OR EXISTS (
				SELECT 1
				FROM knowledge_retry_receipts AS rr
				WHERE rr.event_id = i.event_id
					AND rr.attempt = i.expected_attempt
					AND rr.source_sha256 = i.payload_sha256
			)
		)`).
		Where(`(
			i.decision = ?
			OR d.publication_revision > i.publication_revision
			OR (
				i.decision = ?
				AND d.publication_revision = i.publication_revision
				AND s.applied_publication_revision = i.publication_revision
				AND s.applied_epoch = ?
				AND s.applied_generation_id = ?
				AND i.target_epoch IS NOT NULL
				AND i.target_epoch <= s.applied_epoch
			)
		)`, model.KnowledgeDecisionIgnoreDraft,
			model.KnowledgeDecisionRefresh,
			control.ActiveEpoch, control.ActiveGenerationID).
		Order("i.received_at ASC, i.event_id ASC").
		Limit(terminalBatchSize).
		Find(&rows).Error
	if err != nil {
		return 0, err
	}

	for _, row := range rows {
		outcome := knowledgeevent.ResultApplied
		var generationID *string
		var epoch uint64

		switch {
		case row.Decision == model.KnowledgeDecisionIgnoreDraft:
			outcome = knowledgeevent.ResultIgnoredDraft
		case row.CurrentRevision > row.PublicationRevision:
			outcome = knowledgeevent.ResultSuperseded
		default:
			if control.ActiveGenerationID == nil || control.ActiveEpoch == 0 {
				return 0, knowledgeworker.ErrBuildState
			}
			generationID = control.ActiveGenerationID
			epoch = control.ActiveEpoch
		}

		if err := terminalizeEvent(
			tx, row.KnowledgeEventInbox, outcome, generationID, epoch, now,
		); err != nil {
			return 0, err
		}
	}
	return len(rows), nil
}

func (r *KnowledgeBuildRepository) SettleReady(ctx context.Context) (int, error) {
	count := 0
	err := r.transaction(ctx, func(tx *gorm.DB) error {
		control, err := lockBuildControl(tx)
		if err != nil {
			return err
		}
		now, err := buildDatabaseTime(tx)
		if err != nil {
			return err
		}

		if err := ensureSourceEpochTask(tx, control, now); err != nil {
			return err
		}
		if err := refreshDueEventTargets(tx, control, now); err != nil {
			return err
		}

		if control.ActiveGenerationID != nil {
			var generation model.KnowledgeIndexGeneration
			if err := tx.Where("id = ?", *control.ActiveGenerationID).
				Take(&generation).Error; err != nil {
				return err
			}
			if generation.Status != model.KnowledgeGenerationPublished ||
				generation.SourceEpoch != control.ActiveEpoch ||
				generation.PublishedAt == nil {
				return knowledgeworker.ErrBuildState
			}
			if _, err := generationTarget(generation); err != nil {
				return knowledgeworker.ErrBuildState
			}
		}

		count, err = settleReadyEvents(tx, control, now)
		return err
	})
	if err != nil {
		return 0, err
	}
	return count, nil
}
