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

var _ service.KnowledgeHistoryReader = (*KnowledgeRepository)(nil)

func (r *KnowledgeRepository) GetHistoryByVersionID(ctx context.Context, versionID uint64) (*service.KnowledgeHistoryDetail, error) {
	if versionID == 0 {
		return nil, fmt.Errorf("%w: version_id must not be zero", service.ErrInvalidArgument)
	}

	var result service.KnowledgeHistoryDetail

	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		version, state, err := r.readHistoryVersion(tx, versionID)
		if err != nil {
			return err
		}

		articles := make([]model.RuleArticle, 0)
		err = tx.Where("version_id = ?", version.ID).Order("article_order ASC").Find(&articles).Error
		if err != nil {
			return fmt.Errorf("find historical articles: %w", err)
		}
		if len(articles) == 0 {
			return fmt.Errorf("get knowledge history: published version has no articles")
		}

		result = service.KnowledgeHistoryDetail{
			Version:  version,
			Articles: articles,
			State:    state,
		}
		return nil
	}, &sql.TxOptions{
		Isolation: sql.LevelRepeatableRead,
		ReadOnly:  true,
	})

	if err != nil {
		return nil, fmt.Errorf("get knowledge history transaction: %w", err)
	}

	return &result, nil
}

func (r *KnowledgeRepository) GetHistoryArticle(ctx context.Context, versionID uint64, articleID uint64) (*service.KnowledgeHistoryArticleDetail, error) {
	if versionID == 0 || articleID == 0 {
		return nil, fmt.Errorf("%w: version_id and article_id must not be zero", service.ErrInvalidArgument)
	}

	var result service.KnowledgeHistoryArticleDetail

	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		version, state, err := r.readHistoryVersion(tx, versionID)
		if err != nil {
			return err
		}

		var article model.RuleArticle
		err = tx.Where("id = ? AND version_id = ?", articleID, version.ID).First(&article).Error

		if errors.Is(err, gorm.ErrRecordNotFound) {
			return service.ErrKnowledgeArticleNotFound
		}
		if err != nil {
			return fmt.Errorf("find historical article: %w", err)
		}

		result = service.KnowledgeHistoryArticleDetail{
			Version: version,
			Article: &article,
			State:   state,
		}
		return nil
	}, &sql.TxOptions{
		Isolation: sql.LevelRepeatableRead,
		ReadOnly:  true,
	})

	if err != nil {
		return nil, fmt.Errorf("get knowledge history article transaction: %w", err)
	}

	return &result, nil
}

func (r *KnowledgeRepository) readHistoryVersion(tx *gorm.DB, versionID uint64) (*model.RuleVersion, service.KnowledgeHistoryState, error) {
	var version model.RuleVersion
	var state service.KnowledgeHistoryState

	err := tx.Where("id = ?", versionID).Where("published_at IS NOT NULL").Where("status IN ?", []model.RuleVersionStatus{model.RuleVersionStatusPublished, model.RuleVersionStatusWithdrawn}).First(&version).Error

	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, state, service.ErrKnowledgeVersionNotFound
	}
	if err != nil {
		return nil, state, fmt.Errorf("find historical version: %w", err)
	}

	now := r.clock().UTC().Truncate(time.Millisecond)

	var document model.KnowledgeDocument
	err = tx.Select("id", "current_version_id").Where("id = ?", version.DocumentID).First(&document).Error
	if err != nil {
		return nil, state, fmt.Errorf("find historical version document: %w", err)
	}

	var replacement model.RuleOperation
	err = tx.Select("id").Where("document_id = ? AND operation_type = ?", version.DocumentID, model.RuleOperationTypePublish).Where("before_current_version_id = ?", version.ID).Where("after_current_version_id IS NOT NULL").Where("after_current_version_id <> ?", version.ID).Take(&replacement).Error

	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		state.IsSuperseded = false
	case err != nil:
		return nil, state, fmt.Errorf("find historical replacement operation: %w", err)
	default:
		state.IsSuperseded = true
	}

	state.IsCurrent = document.CurrentVersionID != nil && *document.CurrentVersionID == version.ID

	state.IsExpired = version.ExpiresAt != nil && !now.Before(*version.ExpiresAt)

	state.IsWithdrawn = version.Status == model.RuleVersionStatusWithdrawn || version.WithdrawnAt != nil

	state.IsCurrentEffective = state.IsCurrent && version.Status == model.RuleVersionStatusPublished && !state.IsWithdrawn && version.EffectiveAt != nil && !now.Before(*version.EffectiveAt) && !state.IsExpired

	return &version, state, nil
}
