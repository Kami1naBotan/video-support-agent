package repository

import (
	"context"
	"errors"
	"fmt"
	"video-support-agent/internal/model"
	"video-support-agent/internal/service"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var _ service.KnowledgeDraftEditor = (*KnowledgeRepository)(nil)

func (r *KnowledgeRepository) EditDraftWithVerification(ctx context.Context, versionID uint64, expectedRevision uint64, content model.RuleVersion, articles []model.RuleArticle) (*model.RuleVersion, *model.RuleVerification, error) {
	if versionID == 0 || expectedRevision == 0 {
		return nil, nil, fmt.Errorf("%w: version_id and expected_revision must not be zero", service.ErrInvalidArgument)
	}

	if len(articles) == 0 {
		return nil, nil, fmt.Errorf("%w: articles must contain at least one article", service.ErrInvalidArgument)
	}

	var saved model.RuleVersion
	var verification *model.RuleVerification

	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var version model.RuleVersion

		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", versionID).First(&version).Error

		if errors.Is(err, gorm.ErrRecordNotFound) {
			return service.ErrKnowledgeVersionNotFound
		}
		if err != nil {
			return fmt.Errorf("lock knowledge version: %w", err)
		}

		if version.Status != model.RuleVersionStatusDraft {
			return service.ErrKnowledgeVersionNotEditable
		}

		if version.ContentRevision != expectedRevision {
			return service.ErrKnowledgeContentConflict
		}

		if version.ContentRevision == ^uint64(0) {
			return fmt.Errorf("%w: content revision limit reached", service.ErrKnowledgeContentConflict)
		}

		var category model.RuleCategory

		err = tx.Clauses(clause.Locking{Strength: "SHARE"}).Select("id").Where("id = ?", content.CategoryID).First(&category).Error

		if errors.Is(err, gorm.ErrRecordNotFound) {
			return service.ErrRuleCategoryNotFound
		}
		if err != nil {
			return fmt.Errorf("find rule category: %w", err)
		}

		nextRevision := version.ContentRevision + 1

		updates := map[string]any{
			"category_id":      content.CategoryID,
			"title":            content.Title,
			"content":          content.Content,
			"source_url":       content.SourceURL,
			"effective_at":     content.EffectiveAt,
			"expires_at":       content.ExpiresAt,
			"content_revision": nextRevision,
		}

		result := tx.Model(&model.RuleVersion{}).Where("id = ? AND status = ? AND content_revision = ?", version.ID, model.RuleVersionStatusDraft, expectedRevision).Updates(updates)

		if result.Error != nil {
			return fmt.Errorf("update knowledge version: %w", result.Error)
		}
		if result.RowsAffected != 1 {
			return service.ErrKnowledgeContentConflict
		}

		oldArticleIDs := tx.Model(&model.RuleArticle{}).Select("id").Where("version_id = ?", version.ID)

		if err := tx.Where("article_id IN (?)", oldArticleIDs).Delete(&model.KnowledgeChunk{}).Error; err != nil {
			return fmt.Errorf("delete old knowledge chunks: %w", err)
		}

		if err := tx.Where("version_id = ?", version.ID).Delete(&model.RuleArticle{}).Error; err != nil {
			return fmt.Errorf("delete old rule articles: %w", err)
		}

		for i, inputArticle := range articles {
			article := model.RuleArticle{
				VersionID:     version.ID,
				ArticleOrder:  inputArticle.ArticleOrder,
				SectionPath:   inputArticle.SectionPath,
				ArticleNumber: inputArticle.ArticleNumber,
				Content:       inputArticle.Content,
			}

			if err := tx.Omit(clause.Associations).Create(&article).Error; err != nil {
				return fmt.Errorf("insert rule article %d: %w", i+1, err)
			}

			chunk := model.KnowledgeChunk{
				ArticleID:  article.ID,
				ChunkIndex: 0,
				Content:    article.Content,
			}

			if err := tx.Omit(clause.Associations).Create(&chunk).Error; err != nil {
				return fmt.Errorf("insert knowledge chunk for article %d: %w", i+1, err)
			}
		}

		if err := tx.Where("id = ?", version.ID).First(&saved).Error; err != nil {
			return fmt.Errorf("read saved knowledge version: %w", err)
		}

		var latest model.RuleVerification
		err = tx.Clauses(clause.Locking{Strength: "SHARE"}).Select("id", "version_id", "verified_content_revision", "verified_by", "verified_at", "verification_note", "evidence_url").Where("version_id = ?", version.ID).Order("id DESC").Take(&latest).Error

		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read verification after draft edit: %w", err)
		}

		verification = &latest

		return nil
	})

	if err != nil {
		return nil, nil, fmt.Errorf("edit knowledge draft transaction: %w", err)
	}

	return &saved, verification, nil
}

func (r *KnowledgeRepository) EditDraft(ctx context.Context, versionID uint64, expectedRevision uint64, content model.RuleVersion, articles []model.RuleArticle) (*model.RuleVersion, error) {
	version, _, err := r.EditDraftWithVerification(ctx, versionID, expectedRevision, content, articles)
	return version, err
}
