package repository

import (
	"context"
	"errors"
	"fmt"

	"video-support-agent/internal/model"
	"video-support-agent/internal/service"

	"github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var _ service.KnowledgeRevisionCreator = (*KnowledgeRepository)(nil)

func (r *KnowledgeRepository) CreateRevision(ctx context.Context, version *model.RuleVersion, articles []model.RuleArticle) error {
	if version == nil || version.DocumentID == 0 {
		return fmt.Errorf("%w: version and document_id are required", service.ErrInvalidArgument)
	}

	if len(articles) == 0 {
		return fmt.Errorf("%w: articles must not be empty", service.ErrInvalidArgument)
	}

	saved := model.RuleVersion{
		DocumentID:        version.DocumentID,
		Version:           version.Version,
		ContentRevision:   1,
		CategoryID:        version.CategoryID,
		RevisionType:      version.RevisionType,
		RevisionNote:      version.RevisionNote,
		RevisionSourceURL: version.RevisionSourceURL,
		Title:             version.Title,
		Content:           version.Content,
		SourceURL:         version.SourceURL,
		EffectiveAt:       version.EffectiveAt,
		ExpiresAt:         version.ExpiresAt,
		Status:            model.RuleVersionStatusDraft,
	}

	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var document model.KnowledgeDocument
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Select("id").Where("id = ?", saved.DocumentID).First(&document).Error

		if errors.Is(err, gorm.ErrRecordNotFound) {
			return service.ErrKnowledgeDocumentNotFound
		}
		if err != nil {
			return fmt.Errorf("lock knowledge document: %w", err)
		}

		var category model.RuleCategory
		err = tx.Clauses(clause.Locking{Strength: "SHARE"}).Select("id").Where("id = ?", saved.CategoryID).First(&category).Error

		if errors.Is(err, gorm.ErrRecordNotFound) {
			return service.ErrRuleCategoryNotFound
		}
		if err != nil {
			return fmt.Errorf("find rule category: %w", err)
		}

		if err := tx.Omit(clause.Associations).Create(&saved).Error; err != nil {
			var mysqlErr *mysql.MySQLError
			if errors.As(err, &mysqlErr) && mysqlErr.Number == 1062 {
				return fmt.Errorf("%w: %w", service.ErrKnowledgeVersionExists, err)
			}

			return fmt.Errorf("insert knowledge revision: %w", err)
		}

		for i, source := range articles {
			article := model.RuleArticle{
				VersionID:     saved.ID,
				ArticleOrder:  source.ArticleOrder,
				SectionPath:   source.SectionPath,
				ArticleNumber: source.ArticleNumber,
				Content:       source.Content,
			}

			if err := tx.Omit(clause.Associations).Create(&article).Error; err != nil {
				return fmt.Errorf("insert revision article %d: %w", i+1, err)
			}

			chunk := model.KnowledgeChunk{
				ArticleID:  article.ID,
				ChunkIndex: 0,
				Content:    article.Content,
			}

			if err := tx.Omit(clause.Associations).Create(&chunk).Error; err != nil {
				return fmt.Errorf("insert revision chunk for article %d: %w", i+1, err)
			}
		}

		return nil
	})

	if err != nil {
		return fmt.Errorf("create knowledge revision transaction: %w", err)
	}

	*version = saved
	return nil
}
