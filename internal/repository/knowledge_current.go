package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"video-support-agent/internal/model"
	"video-support-agent/internal/service"

	"gorm.io/gorm"
)

var _ service.KnowledgeCurrentReader = (*KnowledgeRepository)(nil)

func (r *KnowledgeRepository) GetCurrentByDocumentID(ctx context.Context, documentID uint64) (*model.RuleVersion, []model.RuleArticle, error) {
	if documentID == 0 {
		return nil, nil, fmt.Errorf("%w: document_id must not be zero", service.ErrInvalidArgument)
	}

	var version model.RuleVersion
	articles := make([]model.RuleArticle, 0)

	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var document model.KnowledgeDocument
		err := tx.Select("id", "current_version_id").Where("id = ?", documentID).First(&document).Error

		if errors.Is(err, gorm.ErrRecordNotFound) {
			return service.ErrKnowledgeDocumentNotFound
		}
		if err != nil {
			return fmt.Errorf("find current knowledge document: %w", err)
		}

		if document.CurrentVersionID == nil {
			return service.ErrKnowledgeCurrentVersionNotFound
		}

		now := r.clock().UTC().Truncate(time.Millisecond)

		err = tx.
			Where("id = ? AND document_id = ?", *document.CurrentVersionID, document.ID).
			Where("status = ?", model.RuleVersionStatusPublished).
			Where("withdrawn_at IS NULL").
			Where("effective_at IS NOT NULL AND effective_at <= ?", now).
			Where("(expires_at IS NULL OR expires_at > ?)", now).
			First(&version).Error

		if errors.Is(err, gorm.ErrRecordNotFound) {
			return service.ErrKnowledgeCurrentVersionNotFound
		}
		if err != nil {
			return fmt.Errorf("find current effective knowledge version: %w", err)
		}

		err = tx.Where("version_id = ?", version.ID).Order("article_order ASC").Find(&articles).Error
		if err != nil {
			return fmt.Errorf("find current knowledge articles: %w", err)
		}

		if len(articles) == 0 {
			return fmt.Errorf("get current knowledge: published version has no articles")
		}

		return nil
	}, &sql.TxOptions{
		Isolation: sql.LevelRepeatableRead,
		ReadOnly:  true,
	})

	if err != nil {
		return nil, nil, fmt.Errorf("get current knowledge transaction: %w", err)
	}

	return &version, articles, nil
}
