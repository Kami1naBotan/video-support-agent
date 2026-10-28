package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"video-support-agent/internal/model"
	"video-support-agent/internal/service"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var _ service.KnowledgeVersionPublisher = (*KnowledgeRepository)(nil)

func (r *KnowledgeRepository) PublishVersion(ctx context.Context, operatorID uint64, input service.PublishKnowledgeVersionInput) (*service.KnowledgePublicationResult, error) {
	if operatorID == 0 {
		return nil, fmt.Errorf("%w: operator identity is required", service.ErrUnauthorized)
	}
	if input.DocumentID == 0 || input.VersionID == 0 {
		return nil, fmt.Errorf("%w: document_id and version_id must not be zero", service.ErrInvalidArgument)
	}

	var saved service.KnowledgePublicationResult

	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var document model.KnowledgeDocument
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", input.DocumentID).First(&document).Error

		if errors.Is(err, gorm.ErrRecordNotFound) {
			return service.ErrKnowledgeDocumentNotFound
		}
		if err != nil {
			return fmt.Errorf("lock knowledge document: %w", err)
		}

		if document.PublicationRevision != input.ExpectedPublicationRevision {
			return service.ErrKnowledgePublicationConflict
		}
		if document.PublicationRevision == ^uint64(0) {
			return fmt.Errorf("%w: publication revision limit reached", service.ErrKnowledgePublicationConflict)
		}
		var beforeCurrentVersionID *uint64
		if document.CurrentVersionID != nil {
			value := *document.CurrentVersionID
			beforeCurrentVersionID = &value

			if value == input.VersionID {
				return service.ErrKnowledgeVersionNotPublishable
			}
		}

		var version model.RuleVersion
		err = tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND document_id = ?", input.VersionID, document.ID).First(&version).Error

		if errors.Is(err, gorm.ErrRecordNotFound) {
			return service.ErrKnowledgeVersionNotFound
		}
		if err != nil {
			return fmt.Errorf("lock knowledge publication version: %w", err)
		}

		if version.Status != model.RuleVersionStatusDraft {
			return service.ErrKnowledgeVersionNotPublishable
		}

		var verification model.RuleVerification
		err = tx.Clauses(clause.Locking{Strength: "SHARE"}).Select("id", "version_id", "verified_content_revision").Where("version_id = ?", version.ID).Order("id DESC").Take(&verification).Error

		if errors.Is(err, gorm.ErrRecordNotFound) {
			return service.ErrKnowledgeVerificationRequired
		}
		if err != nil {
			return fmt.Errorf("read publication verification: %w", err)
		}
		if verification.VersionID != version.ID || verification.VerifiedContentRevision != version.ContentRevision {
			return service.ErrKnowledgeVerificationRequired
		}

		now := r.clock().UTC().Truncate(time.Millisecond)

		if version.EffectiveAt == nil {
			return service.ErrKnowledgeEffectiveTimeRequired
		}
		if version.EffectiveAt.After(now) {
			return service.ErrKnowledgeNotYetEffective
		}
		if version.ExpiresAt != nil && !now.Before(*version.ExpiresAt) {
			return service.ErrKnowledgeExpired
		}

		nextRevision := document.PublicationRevision + 1

		updatedVersion := tx.Model(&model.RuleVersion{}).
			Where("id = ? AND document_id = ? AND status = ? AND content_revision = ?", version.ID, document.ID, model.RuleVersionStatusDraft, version.ContentRevision).
			Updates(map[string]any{
				"status":       model.RuleVersionStatusPublished,
				"published_at": now,
				"updated_at":   now,
			})

		if updatedVersion.Error != nil {
			return fmt.Errorf("publish knowledge version: %w", updatedVersion.Error)
		}
		if updatedVersion.RowsAffected != 1 {
			return service.ErrKnowledgeVersionNotPublishable
		}

		documentUpdate := tx.Model(&model.KnowledgeDocument{}).Where("id = ? AND publication_revision = ?", document.ID, document.PublicationRevision)

		if beforeCurrentVersionID == nil {
			documentUpdate = documentUpdate.Where("current_version_id IS NULL")
		} else {
			documentUpdate = documentUpdate.Where("current_version_id = ?", *beforeCurrentVersionID)
		}

		updatedDocument := documentUpdate.Updates(map[string]any{
			"current_version_id":   version.ID,
			"publication_revision": nextRevision,
			"updated_at":           now,
		})

		if updatedDocument.Error != nil {
			return fmt.Errorf("update knowledge publication selection: %w", updatedDocument.Error)
		}
		if updatedDocument.RowsAffected != 1 {
			return service.ErrKnowledgePublicationConflict
		}

		targetVersionID := version.ID
		operation := model.RuleOperation{
			DocumentID:                document.ID,
			VersionID:                 version.ID,
			OperationType:             model.RuleOperationTypePublish,
			BeforeCurrentVersionID:    beforeCurrentVersionID,
			AfterCurrentVersionID:     &targetVersionID,
			BeforePublicationRevision: document.PublicationRevision,
			AfterPublicationRevision:  nextRevision,
			OperatorID:                operatorID,
			OperatedAt:                now,
		}

		if err := tx.Omit(clause.Associations).Create(&operation).Error; err != nil {
			return fmt.Errorf("insert publication operation: %w", err)
		}

		saved = service.KnowledgePublicationResult{
			DocumentID:          document.ID,
			VersionID:           version.ID,
			PublicationRevision: nextRevision,
			PublishedAt:         now,
		}
		return nil
	})

	if err != nil {
		return nil, fmt.Errorf("publish knowledge version transaction: %w", err)
	}

	return &saved, nil
}
