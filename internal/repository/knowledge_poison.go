package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"

	"video-support-agent/internal/eventbus"
	"video-support-agent/internal/knowledgeevent"
	"video-support-agent/internal/knowledgeworker"
	"video-support-agent/internal/model"

	"gorm.io/gorm"
)

var _ knowledgeworker.PoisonStore = (*KnowledgeBuildRepository)(nil)

func (r *KnowledgeBuildRepository) IsolatePoison(ctx context.Context, delivery eventbus.Delivery, code string) error {
	notice, err := knowledgeworker.PoisonDescription(delivery, code)
	if err != nil {
		return err
	}
	positionID := knowledgeevent.PoisonPosition(delivery.Topic, delivery.Partition, delivery.Offset)

	conflict := false
	err = r.transaction(ctx, func(tx *gorm.DB) error {
		if _, err := lockBuildControl(tx); err != nil {
			return err
		}

		var existing model.KnowledgePoison
		err := tx.Where("position_id = ?", positionID).Take(&existing).Error
		if err == nil {
			if existing.Topic != delivery.Topic ||
				existing.Partition != delivery.Partition ||
				existing.Offset != delivery.Offset ||
				existing.RecordSHA256 != notice.RecordSHA256 ||
				existing.ReasonCode != code {
				conflict = true
				return knowledgeworker.ErrBuildState
			}

			saved, err := knowledgeevent.DecodePoison(existing.Notice, notice.Key())
			if err != nil || saved.RecordSHA256 != notice.RecordSHA256 || saved.ReasonCode != code {
				return knowledgeworker.ErrBuildState
			}
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}

		return createPoisonIsolation(tx, positionID, notice)
	})

	if ctx.Err() != nil {
		return ctx.Err()
	}
	if conflict {
		return knowledgeworker.ErrPoisonConflict
	}
	if err != nil {
		return knowledgeworker.ErrPoisonStore
	}
	return nil
}

func createPoisonIsolation(tx *gorm.DB, positionID string, notice knowledgeevent.Poison) error {
	now, err := buildDatabaseTime(tx)
	if err != nil {
		return err
	}

	notice.IsolationID, err = knowledgeevent.NewID()
	if err != nil {
		return err
	}
	notice.MessageID, err = knowledgeevent.NewID()
	if err != nil {
		return err
	}
	notice.TraceID, err = knowledgeevent.NewTraceID()
	if err != nil {
		return err
	}
	notice.ObservedAt = knowledgeevent.PoisonTime(now)

	payload, err := knowledgeevent.EncodePoison(notice)
	if err != nil {
		return knowledgeworker.ErrBuildState
	}

	if err := tx.Create(&model.KnowledgePoison{
		PositionID:   positionID,
		Topic:        notice.SourceTopic,
		Partition:    notice.Partition,
		Offset:       notice.Offset,
		RecordSHA256: notice.RecordSHA256,
		ReasonCode:   notice.ReasonCode,
		Notice:       payload,
		CreatedAt:    now,
	}).Error; err != nil {
		return err
	}

	digest := sha256.Sum256(payload)
	return tx.Create(&model.EventOutbox{
		MessageID:     notice.MessageID,
		DedupKey:      notice.Key(),
		EventID:       notice.IsolationID,
		Topic:         eventbus.TopicIndexDLQ,
		MessageKey:    notice.Key(),
		TraceID:       notice.TraceID,
		Payload:       payload,
		PayloadSHA256: hex.EncodeToString(digest[:]),
		Status:        model.OutboxPending,
		NextAttemptAt: now,
		CreatedAt:     now,
		UpdatedAt:     now,
	}).Error
}
