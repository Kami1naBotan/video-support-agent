package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"time"
	"video-support-agent/internal/eventbus"
	"video-support-agent/internal/knowledgeevent"
	"video-support-agent/internal/knowledgefailure"
	"video-support-agent/internal/observability"

	"video-support-agent/internal/knowledgeworker"
	"video-support-agent/internal/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const recoveryBatchSize = 64

const recoveryTimestampLayout = "2006-01-02T15:04:05.000Z"

func bindBuildEvents(tx *gorm.DB, claim knowledgeworker.BuildClaim, now time.Time) error {
	inserted := tx.Exec(`
		INSERT INTO knowledge_build_events
			(generation_id, event_id, attempt, source_sha256, created_at)
		SELECT ?, i.event_id, i.expected_attempt, i.payload_sha256, ?
		FROM knowledge_event_inboxes AS i
		JOIN knowledge_index_attempts AS a
			ON a.event_id = i.event_id
			AND a.attempt = i.expected_attempt
		JOIN knowledge_documents AS d
			ON d.id = i.document_id
		WHERE i.terminal_message_id IS NULL
			AND i.status IN (?, ?)
			AND i.decision = ?
			AND i.target_epoch IS NOT NULL
			AND i.target_epoch <= ?
			AND d.publication_revision = i.publication_revision
			AND a.source_sha256 = i.payload_sha256
			AND a.status IN (?, ?)
			AND a.not_before <= ?
			AND (
				i.expected_attempt = 1
				OR EXISTS (
					SELECT 1
					FROM knowledge_retry_receipts AS rr
					WHERE rr.event_id = i.event_id
						AND rr.attempt = i.expected_attempt
						AND rr.source_sha256 = i.payload_sha256
				)
			)
	`,
		claim.GenerationID, now,
		model.KnowledgeEventReceived,
		model.KnowledgeEventWaiting,
		model.KnowledgeDecisionRefresh,
		claim.SourceEpoch,
		model.KnowledgeAttemptScheduled,
		model.KnowledgeAttemptRunning,
		now,
	)
	if inserted.Error != nil {
		return inserted.Error
	}

	updated := tx.Exec(`
		UPDATE knowledge_index_attempts AS a
		JOIN knowledge_build_events AS b
			ON b.event_id = a.event_id
			AND b.attempt = a.attempt
			AND b.generation_id = ?
		SET a.status = ?,
			a.generation_id = ?,
			a.started_at = ?,
			a.updated_at = ?
		WHERE a.source_sha256 = b.source_sha256
			AND a.status IN (?, ?)
	`,
		claim.GenerationID,
		model.KnowledgeAttemptRunning,
		claim.GenerationID,
		now,
		now,
		model.KnowledgeAttemptScheduled,
		model.KnowledgeAttemptRunning,
	)
	if updated.Error != nil {
		return updated.Error
	}
	if updated.RowsAffected != inserted.RowsAffected {
		return knowledgeworker.ErrBuildState
	}

	return nil
}

func waitingForRecoveryDelivery(tx *gorm.DB, epoch uint64, now time.Time) (bool, error) {
	var row struct {
		EventID string
	}

	err := tx.Table("knowledge_event_inboxes AS i").
		Select("i.event_id").
		Joins(`JOIN knowledge_documents AS d
			ON d.id = i.document_id`).
		Joins(`LEFT JOIN knowledge_index_attempts AS a
			ON a.event_id = i.event_id
			AND a.attempt = i.expected_attempt`).
		Where("i.terminal_message_id IS NULL").
		Where("i.status IN ?", []string{
			model.KnowledgeEventReceived,
			model.KnowledgeEventWaiting,
		}).
		Where("i.decision = ?", model.KnowledgeDecisionRefresh).
		Where("i.target_epoch IS NOT NULL AND i.target_epoch <= ?", epoch).
		Where("d.publication_revision = i.publication_revision").
		Where(`(
			a.event_id IS NULL
			OR a.source_sha256 <> i.payload_sha256
			OR a.status NOT IN (?, ?)
			OR a.not_before > ?
			OR (
				i.expected_attempt > 1
				AND NOT EXISTS (
					SELECT 1
					FROM knowledge_retry_receipts AS rr
					WHERE rr.event_id = i.event_id
						AND rr.attempt = i.expected_attempt
						AND rr.source_sha256 = i.payload_sha256
				)
			)
		)`,
			model.KnowledgeAttemptScheduled,
			model.KnowledgeAttemptRunning,
			now,
		).
		Take(&row).Error

	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func writeRecoveryOutbox(tx *gorm.DB, event model.KnowledgeEventInbox, recovery model.KnowledgeBuildRecovery, decision knowledgefailure.Decision, now time.Time) error {
	messageID, err := knowledgeevent.NewID()
	if err != nil {
		return err
	}

	message := knowledgeevent.Recovery{
		SchemaVersion:  1,
		MessageID:      messageID,
		SourceEventID:  event.EventID,
		DocumentID:     strconv.FormatUint(event.DocumentID, 10),
		SourceSHA256:   event.PayloadSHA256,
		GenerationID:   recovery.GenerationID,
		SourceEpoch:    strconv.FormatUint(recovery.SourceEpoch, 10),
		Kind:           string(decision.Action),
		FailedAttempt:  decision.FailedAttempt,
		NextAttempt:    decision.NextAttempt,
		FailureCode:    decision.FailureCode,
		ReasonCode:     decision.ReasonCode,
		JitterPermille: recovery.JitterPermille,
		DecidedAt:      recovery.DecidedAt.UTC().Format(recoveryTimestampLayout),
		TraceID:        event.TraceID,
	}

	topic := eventbus.TopicIndexDLQ
	if decision.Action == knowledgefailure.ActionRetry {
		topic = eventbus.TopicIndexRetry
		message.NotBefore = decision.NotBefore.UTC().
			Format(recoveryTimestampLayout)
	}

	payload, err := knowledgeevent.EncodeRecovery(message)
	if err != nil {
		return knowledgeworker.ErrBuildState
	}
	digest := sha256.Sum256(payload)

	return tx.Create(&model.EventOutbox{
		MessageID:     messageID,
		DedupKey:      message.DedupKey(),
		EventID:       event.EventID,
		Topic:         topic,
		MessageKey:    message.DocumentID,
		TraceID:       event.TraceID,
		TraceParent:   observability.CaptureTraceParent(tx.Statement.Context, event.TraceParent),
		Payload:       payload,
		PayloadSHA256: hex.EncodeToString(digest[:]),
		Status:        model.OutboxPending,
		NextAttemptAt: now,
		CreatedAt:     now,
		UpdatedAt:     now,
	}).Error
}

func recoverBuildMember(tx *gorm.DB, control model.KnowledgeIndexControl, recovery model.KnowledgeBuildRecovery, member model.KnowledgeBuildEvent, now time.Time) error {
	var event model.KnowledgeEventInbox
	if err := tx.Omit("payload").Where("event_id = ?", member.EventID).Take(&event).Error; err != nil {
		return err
	}

	if event.PayloadSHA256 != member.SourceSHA256 {
		return knowledgeworker.ErrBuildState
	}
	if event.TerminalMessageID != nil {
		return nil
	}
	if event.ExpectedAttempt != member.Attempt || event.Decision != model.KnowledgeDecisionRefresh || event.TargetEpoch == nil || *event.TargetEpoch > recovery.SourceEpoch {
		return knowledgeworker.ErrBuildState
	}

	var document model.KnowledgeDocument
	if err := tx.Select("id", "publication_revision").
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ?", event.DocumentID).
		Take(&document).Error; err != nil {
		return err
	}
	if document.PublicationRevision < event.PublicationRevision {
		return knowledgeworker.ErrBuildState
	}

	var attempt model.KnowledgeIndexAttempt
	if err := tx.Where("event_id = ? AND attempt = ?", event.EventID, member.Attempt).Take(&attempt).Error; err != nil {
		return err
	}

	if attempt.SourceSHA256 != member.SourceSHA256 ||
		attempt.Status != model.KnowledgeAttemptRunning ||
		attempt.GenerationID == nil ||
		*attempt.GenerationID != recovery.GenerationID ||
		attempt.NotBefore.After(now) {
		return knowledgeworker.ErrBuildState
	}

	if document.PublicationRevision > event.PublicationRevision {
		return terminalizeEvent(tx, event, knowledgeevent.ResultSuperseded, nil, 0, now)
	}

	code := recovery.FailureCode
	if recovery.SourceEpoch == control.DesiredEpoch && recovery.ReasonCode == knowledgefailure.ReasonAttemptsExhausted && member.Attempt < knowledgefailure.MaxAttempts {
		code = knowledgefailure.CodeEpochBudgetExhausted
	}

	decision, err := knowledgefailure.Plan(code, member.Attempt, recovery.DecidedAt, recovery.JitterPermille)
	if err != nil {
		return knowledgeworker.ErrBuildState
	}

	if decision.Action == knowledgefailure.ActionRetry {
		if err := persistEventRetry(tx, event, recovery, decision, now); err != nil {
			return err
		}
	} else {
		if err := persistEventDeadLetter(tx, event, recovery, decision, now); err != nil {
			return err
		}
	}

	return writeRecoveryOutbox(tx, event, recovery, decision, now)
}

func persistEventRetry(tx *gorm.DB, event model.KnowledgeEventInbox, recovery model.KnowledgeBuildRecovery, decision knowledgefailure.Decision, now time.Time) error {
	result := tx.Model(&model.KnowledgeIndexAttempt{}).
		Where(
			`event_id = ? AND attempt = ? AND source_sha256 = ?
			 AND generation_id = ? AND status = ?`,
			event.EventID,
			event.ExpectedAttempt,
			event.PayloadSHA256,
			recovery.GenerationID,
			model.KnowledgeAttemptRunning,
		).
		Updates(map[string]any{
			"status":          model.KnowledgeAttemptFailed,
			"safe_error_code": decision.ReasonCode,
			"completed_at":    now,
			"updated_at":      now,
		})
	if err := requireOnePublicationRow(result); err != nil {
		return err
	}

	result = tx.Model(&model.KnowledgeEventInbox{}).
		Where(
			`event_id = ? AND expected_attempt = ?
			 AND terminal_message_id IS NULL AND status IN ?`,
			event.EventID,
			event.ExpectedAttempt,
			[]string{
				model.KnowledgeEventReceived,
				model.KnowledgeEventWaiting,
			},
		).
		Updates(map[string]any{
			"expected_attempt": decision.NextAttempt,
			"status":           model.KnowledgeEventWaiting,
			"safe_error_code":  decision.ReasonCode,
			"updated_at":       now,
		})

	return requireOnePublicationRow(result)
}

func persistEventDeadLetter(tx *gorm.DB, event model.KnowledgeEventInbox, recovery model.KnowledgeBuildRecovery, decision knowledgefailure.Decision, now time.Time) error {
	if err := terminalizeEvent(tx, event, knowledgeevent.ResultDeadLettered, nil, 0, now); err != nil {
		return err
	}

	result := tx.Model(&model.KnowledgeEventInbox{}).
		Where(
			"event_id = ? AND status = ? AND expected_attempt = ?",
			event.EventID,
			model.KnowledgeEventDeadLettered,
			event.ExpectedAttempt,
		).
		Updates(map[string]any{
			"safe_error_code": decision.ReasonCode,
			"updated_at":      now,
		})
	if err := requireOnePublicationRow(result); err != nil {
		return err
	}

	result = tx.Model(&model.KnowledgeIndexAttempt{}).
		Where(
			"event_id = ? AND attempt = ? AND status = ?",
			event.EventID,
			event.ExpectedAttempt,
			model.KnowledgeAttemptFailed,
		).
		Updates(map[string]any{
			"generation_id":   recovery.GenerationID,
			"safe_error_code": decision.ReasonCode,
			"updated_at":      now,
		})
	if err := requireOnePublicationRow(result); err != nil {
		return err
	}

	return tx.Model(&model.KnowledgeDocumentIndex{}).
		Where(
			`document_id = ? AND expected_publication_revision = ?
			 AND expected_epoch <= ? AND status = ?`,
			event.DocumentID,
			event.PublicationRevision,
			recovery.SourceEpoch,
			model.KnowledgeDocumentIndexPending,
		).
		Updates(map[string]any{
			"status":          model.KnowledgeDocumentIndexFailed,
			"safe_error_code": decision.ReasonCode,
			"updated_at":      now,
		}).Error
}

func (r *KnowledgeBuildRepository) RecoverPending(ctx context.Context) (int, error) {
	work := 0

	err := r.transaction(ctx, func(tx *gorm.DB) error {
		control, err := lockBuildControl(tx)
		if err != nil {
			return err
		}

		var recovery model.KnowledgeBuildRecovery
		err = tx.Where("status = ?", model.KnowledgeRecoveryPending).
			Order("decided_at ASC, generation_id ASC").
			Take(&recovery).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := validateBuildRecovery(recovery); err != nil {
			return err
		}
		if control.OwnerToken != nil ||
			recovery.SourceEpoch > control.DesiredEpoch {
			return knowledgeworker.ErrBuildState
		}

		var generation model.KnowledgeIndexGeneration
		if err := tx.Where("id = ?", recovery.GenerationID).
			Take(&generation).Error; err != nil {
			return err
		}
		if generation.Status != model.KnowledgeGenerationAbandoned ||
			generation.SourceEpoch != recovery.SourceEpoch ||
			generation.Attempt != recovery.FailedAttempt ||
			generation.SafeErrorCode != recovery.FailureCode {
			return knowledgeworker.ErrBuildState
		}
		if control.ActiveGenerationID != nil && *control.ActiveGenerationID == generation.ID {
			return knowledgeworker.ErrBuildState
		}

		var task model.KnowledgeIndexTask
		if err := tx.Where("source_epoch = ?", recovery.SourceEpoch).
			Take(&task).Error; err != nil {
			return err
		}
		if task.GenerationID == nil ||
			*task.GenerationID != recovery.GenerationID ||
			task.FailedAttempts != recovery.FailedAttempt ||
			(task.Status != model.KnowledgeTaskBlocked && task.Status != model.KnowledgeTaskObsolete) {
			return knowledgeworker.ErrBuildState
		}

		now, err := buildDatabaseTime(tx)
		if err != nil {
			return err
		}

		var members []model.KnowledgeBuildEvent
		if err := tx.
			Where("generation_id = ? AND recovery_processed_at IS NULL", recovery.GenerationID).
			Order("event_id ASC").
			Limit(recoveryBatchSize).
			Find(&members).Error; err != nil {
			return err
		}

		for _, member := range members {
			if err := recoverBuildMember(
				tx, control, recovery, member, now,
			); err != nil {
				return err
			}

			result := tx.Model(&model.KnowledgeBuildEvent{}).
				Where(
					`generation_id = ? AND event_id = ?
					 AND recovery_processed_at IS NULL`,
					recovery.GenerationID,
					member.EventID,
				).
				Update("recovery_processed_at", now)
			if err := requireOnePublicationRow(result); err != nil {
				return err
			}
			work++
		}

		var remaining model.KnowledgeBuildEvent
		err = tx.Select("generation_id", "event_id").
			Where("generation_id = ? AND recovery_processed_at IS NULL", recovery.GenerationID).
			Take(&remaining).Error
		if err == nil {
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}

		if err := finishBuildRecovery(
			tx, control, recovery, now,
		); err != nil {
			return err
		}
		work++
		return nil
	})
	if err != nil {
		return 0, err
	}
	return work, nil
}

func finishBuildRecovery(tx *gorm.DB, control model.KnowledgeIndexControl, recovery model.KnowledgeBuildRecovery, now time.Time) error {
	status := model.KnowledgeTaskBlocked
	notBefore := recovery.DecidedAt
	var completedAt *time.Time

	if recovery.SourceEpoch < control.DesiredEpoch {
		status = model.KnowledgeTaskObsolete
		completedAt = &now
	} else if recovery.Action == string(knowledgefailure.ActionRetry) {
		if recovery.NotBefore == nil {
			return knowledgeworker.ErrBuildState
		}
		status = model.KnowledgeTaskRetryWait
		notBefore = *recovery.NotBefore
	}

	if err := tx.Model(&model.KnowledgeIndexTask{}).
		Where(
			"source_epoch = ? AND generation_id = ? AND failed_attempts = ?",
			recovery.SourceEpoch,
			recovery.GenerationID,
			recovery.FailedAttempt,
		).
		Updates(map[string]any{
			"status":          status,
			"not_before":      notBefore,
			"safe_error_code": recovery.ReasonCode,
			"completed_at":    completedAt,
			"updated_at":      now,
		}).Error; err != nil {
		return err
	}

	result := tx.Model(&model.KnowledgeBuildRecovery{}).
		Where(
			"generation_id = ? AND status = ?",
			recovery.GenerationID,
			model.KnowledgeRecoveryPending,
		).
		Updates(map[string]any{
			"status":       model.KnowledgeRecoveryCompleted,
			"completed_at": now,
			"updated_at":   now,
		})

	return requireOnePublicationRow(result)
}
