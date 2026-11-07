package repository

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"time"
	"video-support-agent/internal/observability"

	"video-support-agent/internal/eventbus"
	"video-support-agent/internal/knowledgeevent"
	"video-support-agent/internal/knowledgeworker"
	"video-support-agent/internal/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

type KnowledgeInboxRepository struct {
	db *gorm.DB
}

var _ knowledgeworker.SourceStore = (*KnowledgeInboxRepository)(nil)

func NewKnowledgeInboxRepository(db *gorm.DB) (*KnowledgeInboxRepository, error) {
	if db == nil {
		return nil, knowledgeworker.ErrSourceConfig
	}
	return &KnowledgeInboxRepository{db: db}, nil
}

func (r *KnowledgeInboxRepository) AcceptSource(ctx context.Context, delivery eventbus.Delivery) error {
	if r == nil || r.db == nil {
		return knowledgeworker.ErrSourceConfig
	}

	source, err := knowledgeworker.DecodeSourceDelivery(delivery)
	if err != nil {
		return err
	}

	traced, err := knowledgeevent.WithTrace(ctx, source.TraceID)
	if err != nil {
		return knowledgeworker.ErrSourceRecord
	}
	bounded, cancel := context.WithTimeout(traced, 8*time.Second)
	defer cancel()

	digest := sha256.Sum256(delivery.Value)
	hash := hex.EncodeToString(digest[:])

	db := r.db.Session(&gorm.Session{Logger: logger.Default.LogMode(logger.Silent)}).WithContext(bounded)

	err = db.Transaction(func(tx *gorm.DB) error {
		return acceptSourceTransaction(tx, source, delivery, hash)
	})
	if err == nil {
		return bounded.Err()
	}
	if bounded.Err() != nil {
		return bounded.Err()
	}
	if errors.Is(err, knowledgeworker.ErrSourceConflict) || errors.Is(err, knowledgeworker.ErrSourceState) {
		return err
	}
	return knowledgeworker.ErrSourceStore
}

func acceptSourceTransaction(tx *gorm.DB, source knowledgeevent.Source, delivery eventbus.Delivery, hash string) error {
	var control model.KnowledgeIndexControl
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", model.KnowledgeIndexControlID).Take(&control).Error; err != nil {
		return err
	}
	if control.DesiredEpoch == 0 ||
		control.ActiveEpoch > control.DesiredEpoch ||
		(control.ActiveEpoch == 0 && control.ActiveGenerationID != nil) ||
		(control.ActiveEpoch > 0 && control.ActiveGenerationID == nil) {
		return knowledgeworker.ErrSourceState
	}

	var clock struct {
		Now time.Time
	}
	if err := tx.Raw("SELECT UTC_TIMESTAMP(3) AS now").Scan(&clock).Error; err != nil {
		return err
	}
	if clock.Now.IsZero() {
		return knowledgeworker.ErrSourceState
	}
	now := clock.Now.UTC()

	var existing model.KnowledgeEventInbox
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("event_id = ?", source.EventID).Take(&existing).Error
	if err == nil {
		if existing.PayloadSHA256 != hash || !bytes.Equal(existing.Payload, delivery.Value) {
			return knowledgeworker.ErrSourceConflict
		}

		var attempt model.KnowledgeIndexAttempt
		if err := tx.Where("event_id = ? AND attempt = ?", source.EventID, 1).Take(&attempt).Error; err != nil {
			return err
		}
		if attempt.SourceSHA256 != hash {
			return knowledgeworker.ErrSourceConflict
		}
		if sourceNeedsIndex(source.EventType) {
			return ensureSourceEpochTask(tx, control, now)
		}
		return nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}

	document, decision, err := classifySource(tx, source)
	if err != nil {
		return err
	}

	status := model.KnowledgeEventReceived
	var targetEpoch *uint64

	if sourceNeedsIndex(source.EventType) {
		epoch := control.DesiredEpoch
		targetEpoch = &epoch

		if err := ensureSourceEpochTask(tx, control, now); err != nil {
			return err
		}
		if control.ActiveEpoch < control.DesiredEpoch {
			if err := expectDocumentIndex(tx, document, epoch, now); err != nil {
				return err
			}
			if decision == model.KnowledgeDecisionRefresh {
				status = model.KnowledgeEventWaiting
			}
		}
	}

	versionID, _ := strconv.ParseUint(source.VersionID, 10, 64)
	publicationRevision, _ := strconv.ParseUint(source.PublicationRevision, 10, 64)

	inbox := model.KnowledgeEventInbox{
		EventID:             source.EventID,
		EventType:           source.EventType,
		DocumentID:          document.ID,
		VersionID:           versionID,
		PublicationRevision: publicationRevision,
		TraceID:             source.TraceID,
		TraceParent:         observability.CaptureTraceParent(tx.Statement.Context),
		Payload:             bytes.Clone(delivery.Value),
		PayloadSHA256:       hash,
		TargetEpoch:         targetEpoch,
		ExpectedAttempt:     1,
		Status:              status,
		Decision:            decision,
		ReceivedAt:          now,
		CreatedAt:           now,
		UpdatedAt:           now,
	}
	if err := tx.Create(&inbox).Error; err != nil {
		return err
	}

	attempt := model.KnowledgeIndexAttempt{
		EventID:      source.EventID,
		Attempt:      1,
		SourceSHA256: hash,
		TraceParent:  observability.CaptureTraceParent(tx.Statement.Context),
		Topic:        delivery.Topic,
		Partition:    delivery.Partition,
		Offset:       delivery.Offset,
		TargetEpoch:  targetEpoch,
		Status:       model.KnowledgeAttemptScheduled,
		NotBefore:    now,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	return tx.Create(&attempt).Error
}

func sourceNeedsIndex(eventType string) bool {
	return eventType == knowledgeevent.VersionPublished ||
		eventType == knowledgeevent.VersionWithdrawn
}

func classifySource(tx *gorm.DB, source knowledgeevent.Source) (model.KnowledgeDocument, string, error) {
	documentID, _ := strconv.ParseUint(source.DocumentID, 10, 64)
	versionID, _ := strconv.ParseUint(source.VersionID, 10, 64)
	contentRevision, _ := strconv.ParseUint(source.ContentRevision, 10, 64)
	publicationRevision, _ := strconv.ParseUint(source.PublicationRevision, 10, 64)

	var document model.KnowledgeDocument
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ?", documentID).Take(&document).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return document, "", knowledgeworker.ErrSourceState
		}
		return document, "", err
	}

	var version model.RuleVersion
	if err := tx.Select(
		"id", "document_id", "content_revision",
		"status", "published_at", "withdrawn_at",
	).Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ? AND document_id = ?", versionID, documentID).
		Take(&version).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return document, "", knowledgeworker.ErrSourceState
		}
		return document, "", err
	}

	if publicationRevision > document.PublicationRevision ||
		contentRevision > version.ContentRevision {
		return document, "", knowledgeworker.ErrSourceState
	}

	if !sourceNeedsIndex(source.EventType) {
		return document, model.KnowledgeDecisionIgnoreDraft, nil
	}

	if contentRevision != version.ContentRevision {
		return document, "", knowledgeworker.ErrSourceState
	}
	if publicationRevision < document.PublicationRevision {
		return document, model.KnowledgeDecisionSupersede, nil
	}

	switch source.EventType {
	case knowledgeevent.VersionPublished:
		if document.CurrentVersionID == nil ||
			*document.CurrentVersionID != version.ID ||
			version.Status != model.RuleVersionStatusPublished ||
			version.PublishedAt == nil || version.WithdrawnAt != nil {
			return document, "", knowledgeworker.ErrSourceState
		}
	case knowledgeevent.VersionWithdrawn:
		if document.CurrentVersionID != nil ||
			version.Status != model.RuleVersionStatusWithdrawn ||
			version.PublishedAt == nil || version.WithdrawnAt == nil {
			return document, "", knowledgeworker.ErrSourceState
		}
	}

	return document, model.KnowledgeDecisionRefresh, nil
}

func ensureSourceEpochTask(tx *gorm.DB, control model.KnowledgeIndexControl, now time.Time) error {
	if control.ActiveEpoch == control.DesiredEpoch {
		return nil
	}

	task := model.KnowledgeIndexTask{
		SourceEpoch: control.DesiredEpoch,
		Status:      model.KnowledgeTaskPending,
		NotBefore:   now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	return tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&task).Error
}

func expectDocumentIndex(tx *gorm.DB, document model.KnowledgeDocument, epoch uint64, now time.Time) error {
	var state model.KnowledgeDocumentIndex
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("document_id = ?", document.ID).Take(&state).Error

	if errors.Is(err, gorm.ErrRecordNotFound) {
		state = model.KnowledgeDocumentIndex{
			DocumentID:                  document.ID,
			ExpectedPublicationRevision: document.PublicationRevision,
			ExpectedEpoch:               epoch,
			Status:                      model.KnowledgeDocumentIndexPending,
			CreatedAt:                   now,
			UpdatedAt:                   now,
		}
		return tx.Create(&state).Error
	}
	if err != nil {
		return err
	}

	if state.ExpectedPublicationRevision > document.PublicationRevision ||
		state.ExpectedEpoch > epoch {
		return knowledgeworker.ErrSourceState
	}
	if state.ExpectedPublicationRevision == document.PublicationRevision &&
		state.ExpectedEpoch == epoch {
		return nil
	}

	return tx.Model(&model.KnowledgeDocumentIndex{}).
		Where("document_id = ?", document.ID).
		Updates(map[string]any{
			"expected_publication_revision": document.PublicationRevision,
			"expected_epoch":                epoch,
			"status":                        model.KnowledgeDocumentIndexPending,
			"safe_error_code":               "",
			"updated_at":                    now,
		}).Error
}
