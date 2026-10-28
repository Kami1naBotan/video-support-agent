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

var _ service.KnowledgeVersionWithdrawer = (*KnowledgeRepository)(nil)

func (r *KnowledgeRepository) WithdrawVersion(ctx context.Context, operatorID uint64, input service.WithdrawKnowledgeVersionInput) (*service.KnowledgeWithdrawalResult, error) {
	if operatorID == 0 {
		return nil, fmt.Errorf("%w: operator identity is required", service.ErrUnauthorized)
	}
	if input.DocumentID == 0 || input.VersionID == 0 {
		return nil, fmt.Errorf("%w: document_id and version_id must not be zero", service.ErrInvalidArgument)
	}

	var saved service.KnowledgeWithdrawalResult

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

		var version model.RuleVersion
		err = tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND document_id = ?", input.VersionID, document.ID).First(&version).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return service.ErrKnowledgeVersionNotFound
		}
		if err != nil {
			return fmt.Errorf("lock knowledge withdrawal version: %w", err)
		}

		if document.CurrentVersionID == nil || *document.CurrentVersionID != version.ID {
			return service.ErrKnowledgeVersionNotWithdrawable
		}
		if version.Status != model.RuleVersionStatusPublished || version.PublishedAt == nil || version.WithdrawnAt != nil {
			return service.ErrKnowledgeVersionNotWithdrawable
		}

		now := r.clock().UTC().Truncate(time.Millisecond)
		nextRevision := document.PublicationRevision + 1

		updatedVersion := tx.Model(&model.RuleVersion{}).
			Where("id = ? AND document_id = ?", version.ID, document.ID).
			Where("status = ?", model.RuleVersionStatusPublished).
			Where("published_at IS NOT NULL AND withdrawn_at IS NULL").
			Updates(map[string]any{
				"status":       model.RuleVersionStatusWithdrawn,
				"withdrawn_at": now,
				"updated_at":   now,
			})
		if updatedVersion.Error != nil {
			return fmt.Errorf("withdraw knowledge version: %w", updatedVersion.Error)
		}
		if updatedVersion.RowsAffected != 1 {
			return service.ErrKnowledgeVersionNotWithdrawable
		}

		updatedDocument := tx.Model(&model.KnowledgeDocument{}).
			Where("id = ? AND publication_revision = ?", document.ID, document.PublicationRevision).
			Where("current_version_id = ?", version.ID).
			Updates(map[string]any{
				"current_version_id":   nil,
				"publication_revision": nextRevision,
				"updated_at":           now,
			})
		if updatedDocument.Error != nil {
			return fmt.Errorf("clear knowledge publication selection: %w", updatedDocument.Error)
		}
		if updatedDocument.RowsAffected != 1 {
			return service.ErrKnowledgePublicationConflict
		}

		beforeCurrentVersionID := version.ID
		operation := model.RuleOperation{
			DocumentID:                document.ID,
			VersionID:                 version.ID,
			OperationType:             model.RuleOperationTypeWithdraw,
			BeforeCurrentVersionID:    &beforeCurrentVersionID,
			AfterCurrentVersionID:     nil,
			BeforePublicationRevision: document.PublicationRevision,
			AfterPublicationRevision:  nextRevision,
			OperatorID:                operatorID,
			OperatedAt:                now,
		}
		if err := tx.Omit(clause.Associations).Create(&operation).Error; err != nil {
			return fmt.Errorf("insert withdrawal operation: %w", err)
		}

		saved = service.KnowledgeWithdrawalResult{
			DocumentID:          document.ID,
			VersionID:           version.ID,
			PublicationRevision: nextRevision,
			WithdrawnAt:         now,
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("withdraw knowledge version transaction: %w", err)
	}

	return &saved, nil
}
