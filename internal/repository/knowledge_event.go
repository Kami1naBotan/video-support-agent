package repository

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"
	"video-support-agent/internal/observability"

	"video-support-agent/internal/eventbus"
	"video-support-agent/internal/knowledgeevent"
	"video-support-agent/internal/model"

	"gorm.io/gorm"
)

func requireEventTransaction(tx *gorm.DB) error {
	if tx == nil || tx.Statement == nil {
		return errors.New("knowledge event requires a transaction")
	}
	if _, ok := tx.Statement.ConnPool.(gorm.TxCommitter); !ok {
		return errors.New("knowledge event requires a transaction")
	}
	if tx.Error != nil {
		return tx.Error
	}
	return nil
}

func (r *KnowledgeRepository) appendSourceEvent(tx *gorm.DB, eventType string, documentID, versionID, contentRevision, publicationRevision uint64) error {
	if err := requireEventTransaction(tx); err != nil {
		return err
	}

	_, trace, err := knowledgeevent.EnsureTrace(tx.Statement.Context)
	if err != nil {
		return err
	}
	eventID, err := knowledgeevent.NewID()
	if err != nil {
		return err
	}

	messageID, err := knowledgeevent.NewID()
	if err != nil {
		return err
	}

	now := r.clock().UTC().Truncate(time.Millisecond)
	source := knowledgeevent.Source{
		SchemaVersion:       1,
		EventID:             eventID,
		EventType:           eventType,
		DocumentID:          strconv.FormatUint(documentID, 10),
		VersionID:           strconv.FormatUint(versionID, 10),
		ContentRevision:     strconv.FormatUint(contentRevision, 10),
		PublicationRevision: strconv.FormatUint(publicationRevision, 10),
		OccurredAt:          now.Format(time.RFC3339Nano),
		TraceID:             trace,
	}

	payload, err := knowledgeevent.Encode(source)
	if err != nil {
		return err
	}

	digest := sha256.Sum256(payload)
	row := model.EventOutbox{
		MessageID:     messageID,
		DedupKey:      "source:" + eventID,
		EventID:       eventID,
		Topic:         eventbus.TopicDocumentEvents,
		MessageKey:    source.DocumentID,
		TraceID:       trace,
		TraceParent:   observability.CaptureTraceParent(tx.Statement.Context),
		Payload:       payload,
		PayloadSHA256: hex.EncodeToString(digest[:]),
		Status:        model.OutboxPending,
		NextAttemptAt: now,
		CreatedAt:     now,
		UpdatedAt:     now,
	}

	if err := tx.Create(&row).Error; err != nil {
		return fmt.Errorf("insert knowledge source outboxrelay: %w", err)
	}

	return nil
}
