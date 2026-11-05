package repository

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"time"

	"video-support-agent/internal/eventbus"
	"video-support-agent/internal/knowledgeevent"
	"video-support-agent/internal/knowledgeworker"
	"video-support-agent/internal/model"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var _ knowledgeworker.RetryStore = (*KnowledgeBuildRepository)(nil)

func (r *KnowledgeBuildRepository) AcceptRetry(ctx context.Context, delivery eventbus.Delivery) error {
	if r == nil || r.db == nil {
		return knowledgeworker.ErrRetryConfig
	}

	message, err := knowledgeworker.DecodeRetryDelivery(delivery)
	if err != nil {
		return err
	}

	traced, err := knowledgeevent.WithTrace(ctx, message.TraceID)
	if err != nil {
		return knowledgeworker.ErrRetryRecord
	}

	bounded, cancel := context.WithTimeout(traced, 8*time.Second)
	defer cancel()

	digest := sha256.Sum256(delivery.Value)
	hash := hex.EncodeToString(digest[:])

	db := r.db.Session(&gorm.Session{Logger: logger.Default.LogMode(logger.Silent)}).WithContext(bounded)

	err = db.Transaction(func(tx *gorm.DB) error {
		return acceptRetryTransaction(tx, message, delivery, hash)
	})

	if ctx.Err() != nil {
		return ctx.Err()
	}
	if bounded.Err() != nil {
		return knowledgeworker.ErrRetryStore
	}
	if err == nil {
		return nil
	}

	for _, known := range []error{
		knowledgeworker.ErrRetryRecord,
		knowledgeworker.ErrRetryConflict,
		knowledgeworker.ErrRetryState,
	} {
		if errors.Is(err, known) {
			return known
		}
	}
	return knowledgeworker.ErrRetryStore
}

func acceptRetryTransaction(tx *gorm.DB, message knowledgeevent.Recovery, delivery eventbus.Delivery, hash string) error {
	control, err := lockBuildControl(tx)
	if err != nil {
		return err
	}

	var original model.EventOutbox
	err = tx.Where("message_id = ?", message.MessageID).Take(&original).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return knowledgeworker.ErrRetryConflict
	}
	if err != nil {
		return err
	}

	if original.Topic != eventbus.TopicIndexRetry ||
		original.EventID != message.SourceEventID ||
		original.MessageKey != message.DocumentID ||
		original.TraceID != message.TraceID ||
		original.DedupKey != message.DedupKey() ||
		original.PayloadSHA256 != hash ||
		!bytes.Equal(original.Payload, delivery.Value) {
		return knowledgeworker.ErrRetryConflict
	}

	var receipts []model.KnowledgeRetryReceipt
	if err := tx.Where(
		"message_id = ? OR (event_id = ? AND attempt = ?)",
		message.MessageID,
		message.SourceEventID,
		message.NextAttempt,
	).Limit(2).Find(&receipts).Error; err != nil {
		return err
	}

	if len(receipts) > 0 {
		if len(receipts) != 1 {
			return knowledgeworker.ErrRetryConflict
		}
		receipt := receipts[0]
		if receipt.MessageID != message.MessageID ||
			receipt.EventID != message.SourceEventID ||
			receipt.Attempt != message.NextAttempt ||
			receipt.PayloadSHA256 != hash ||
			receipt.SourceSHA256 != message.SourceSHA256 ||
			receipt.Topic != delivery.Topic {
			return knowledgeworker.ErrRetryConflict
		}
		return nil
	}

	return acceptNewRetry(tx, control, message, delivery, hash)
}

func acceptNewRetry(tx *gorm.DB, control model.KnowledgeIndexControl, message knowledgeevent.Recovery, delivery eventbus.Delivery, hash string) error {
	var event model.KnowledgeEventInbox
	if err := tx.Omit("payload").
		Where("event_id = ?", message.SourceEventID).
		Take(&event).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return knowledgeworker.ErrRetryState
		}
		return err
	}

	if strconv.FormatUint(event.DocumentID, 10) != message.DocumentID || event.PayloadSHA256 != message.SourceSHA256 || event.TraceID != message.TraceID {
		return knowledgeworker.ErrRetryConflict
	}

	epoch, err := strconv.ParseUint(message.SourceEpoch, 10, 64)
	if err != nil || epoch > control.DesiredEpoch {
		return knowledgeworker.ErrRetryState
	}

	var recovery model.KnowledgeBuildRecovery
	if err := tx.Where("generation_id = ?", message.GenerationID).
		Take(&recovery).Error; err != nil {
		return err
	}
	if err := validateBuildRecovery(recovery); err != nil {
		return knowledgeworker.ErrRetryState
	}
	if recovery.SourceEpoch != epoch ||
		recovery.FailureCode != message.FailureCode ||
		recovery.JitterPermille != message.JitterPermille ||
		recovery.DecidedAt.UTC().Format(recoveryTimestampLayout) != message.DecidedAt {
		return knowledgeworker.ErrRetryState
	}

	var member model.KnowledgeBuildEvent
	if err := tx.Where("generation_id = ? AND event_id = ?", message.GenerationID, message.SourceEventID).Take(&member).Error; err != nil {
		return err
	}
	if member.Attempt != message.FailedAttempt || member.SourceSHA256 != message.SourceSHA256 || member.RecoveryProcessedAt == nil {
		return knowledgeworker.ErrRetryState
	}

	var failed model.KnowledgeIndexAttempt
	if err := tx.Where("event_id = ? AND attempt = ?", event.EventID, message.FailedAttempt).Take(&failed).Error; err != nil {
		return err
	}
	if failed.SourceSHA256 != message.SourceSHA256 ||
		failed.Status != model.KnowledgeAttemptFailed ||
		failed.GenerationID == nil ||
		*failed.GenerationID != message.GenerationID ||
		failed.CompletedAt == nil ||
		failed.SafeErrorCode != message.ReasonCode {
		return knowledgeworker.ErrRetryState
	}

	return persistRetryReceiptAndAttempt(tx, control, event, message, delivery, hash)
}

func persistRetryReceiptAndAttempt(tx *gorm.DB, control model.KnowledgeIndexControl, event model.KnowledgeEventInbox, message knowledgeevent.Recovery, delivery eventbus.Delivery, hash string) error {
	now, err := buildDatabaseTime(tx)
	if err != nil {
		return err
	}

	receipt := model.KnowledgeRetryReceipt{
		MessageID:     message.MessageID,
		EventID:       message.SourceEventID,
		Attempt:       message.NextAttempt,
		PayloadSHA256: hash,
		SourceSHA256:  message.SourceSHA256,
		Topic:         delivery.Topic,
		Partition:     delivery.Partition,
		Offset:        delivery.Offset,
		ReceivedAt:    now,
	}

	if event.TerminalMessageID != nil {
		return tx.Create(&receipt).Error
	}

	if event.ExpectedAttempt != message.NextAttempt ||
		event.Decision != model.KnowledgeDecisionRefresh ||
		event.TargetEpoch == nil ||
		(event.Status != model.KnowledgeEventReceived && event.Status != model.KnowledgeEventWaiting) {
		return knowledgeworker.ErrRetryState
	}

	notBefore, err := time.Parse(recoveryTimestampLayout, message.NotBefore)
	if err != nil {
		return knowledgeworker.ErrRetryRecord
	}

	var existing model.KnowledgeIndexAttempt
	err = tx.Where("event_id = ? AND attempt = ?", event.EventID, message.NextAttempt).Take(&existing).Error
	if err == nil {
		return knowledgeworker.ErrRetryState
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}

	epoch := control.DesiredEpoch
	attempt := model.KnowledgeIndexAttempt{
		EventID:      event.EventID,
		Attempt:      message.NextAttempt,
		SourceSHA256: event.PayloadSHA256,
		Topic:        delivery.Topic,
		Partition:    delivery.Partition,
		Offset:       delivery.Offset,
		TargetEpoch:  &epoch,
		Status:       model.KnowledgeAttemptScheduled,
		NotBefore:    notBefore,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := tx.Create(&attempt).Error; err != nil {
		return err
	}

	result := tx.Model(&model.KnowledgeEventInbox{}).
		Where(
			`event_id = ? AND expected_attempt = ?
			 AND terminal_message_id IS NULL`,
			event.EventID,
			message.NextAttempt,
		).
		Updates(map[string]any{
			"target_epoch": epoch,
			"status":       model.KnowledgeEventWaiting,
			"updated_at":   now,
		})
	if err := requireOnePublicationRow(result); err != nil {
		return err
	}

	if err := ensureSourceEpochTask(tx, control, now); err != nil {
		return err
	}

	return tx.Create(&receipt).Error
}

func refreshDueEventTargets(tx *gorm.DB, control model.KnowledgeIndexControl, now time.Time) error {
	type dueEvent struct {
		EventID            string
		ExpectedAttempt    uint32
		TargetEpoch        *uint64
		AttemptTargetEpoch *uint64
		SafeErrorCode      string
	}

	var rows []dueEvent
	err := tx.Table("knowledge_event_inboxes AS i").
		Select(`i.event_id, i.expected_attempt, i.target_epoch,
			a.target_epoch AS attempt_target_epoch,
			i.safe_error_code`).
		Joins(`JOIN knowledge_index_attempts AS a
			ON a.event_id = i.event_id
			AND a.attempt = i.expected_attempt`).
		Joins(`JOIN knowledge_documents AS d
			ON d.id = i.document_id`).
		Where("i.terminal_message_id IS NULL").
		Where("i.status IN ?", []string{
			model.KnowledgeEventReceived,
			model.KnowledgeEventWaiting,
		}).
		Where("i.decision = ?", model.KnowledgeDecisionRefresh).
		Where("d.publication_revision = i.publication_revision").
		Where(
			"a.status = ? AND a.not_before <= ?",
			model.KnowledgeAttemptScheduled,
			now,
		).
		Where("a.source_sha256 = i.payload_sha256").
		Where(`(
			i.expected_attempt = 1
			OR EXISTS (
				SELECT 1 FROM knowledge_retry_receipts AS rr
				WHERE rr.event_id = i.event_id
					AND rr.attempt = i.expected_attempt
					AND rr.source_sha256 = i.payload_sha256
			)
		)`).
		Order("i.updated_at ASC, i.event_id ASC").
		Limit(terminalBatchSize).
		Find(&rows).Error
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}

	code := ""
	if control.ActiveEpoch < control.DesiredEpoch {
		var task model.KnowledgeIndexTask
		if err := tx.Where("source_epoch = ?", control.DesiredEpoch).
			Take(&task).Error; err != nil {
			return err
		}
		if task.Status == model.KnowledgeTaskBlocked {
			code = task.SafeErrorCode
		}
	}

	for _, row := range rows {
		if row.TargetEpoch == nil ||
			row.AttemptTargetEpoch == nil ||
			*row.TargetEpoch > control.DesiredEpoch ||
			*row.AttemptTargetEpoch > control.DesiredEpoch {
			return knowledgeworker.ErrBuildState
		}

		if err := tx.Model(&model.KnowledgeIndexAttempt{}).
			Where(
				"event_id = ? AND attempt = ? AND status = ?",
				row.EventID,
				row.ExpectedAttempt,
				model.KnowledgeAttemptScheduled,
			).
			Updates(map[string]any{
				"target_epoch": control.DesiredEpoch,
				"updated_at":   now,
			}).Error; err != nil {
			return err
		}

		if err := tx.Model(&model.KnowledgeEventInbox{}).
			Where(
				`event_id = ? AND expected_attempt = ?
				 AND terminal_message_id IS NULL`,
				row.EventID,
				row.ExpectedAttempt,
			).
			Updates(map[string]any{
				"target_epoch":    control.DesiredEpoch,
				"safe_error_code": code,
				"updated_at":      now,
			}).Error; err != nil {
			return err
		}
	}

	return nil
}
