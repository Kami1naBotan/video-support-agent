package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
	"video-support-agent/internal/model"
	"video-support-agent/internal/service"

	"github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Clock func() time.Time

type KnowledgeRepository struct {
	db    *gorm.DB
	clock Clock
}

func NewKnowledgeRepository(db *gorm.DB) *KnowledgeRepository {
	return NewKnowledgeRepositoryWithClock(db, time.Now)
}

func NewKnowledgeRepositoryWithClock(db *gorm.DB, clock Clock) *KnowledgeRepository {
	if clock == nil {
		panic("repository: knowledge clock must not be nil")
	}

	return &KnowledgeRepository{
		db:    db,
		clock: clock,
	}
}

func (r *KnowledgeRepository) CreateDraft(ctx context.Context, document *model.KnowledgeDocument, version *model.RuleVersion, articles []model.RuleArticle) error {
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var category model.RuleCategory

		err := tx.Clauses(clause.Locking{Strength: "SHARE"}).Select("id").First(&category, version.CategoryID).Error

		if errors.Is(err, gorm.ErrRecordNotFound) {
			return fmt.Errorf("%w: category_id-%d", service.ErrRuleCategoryNotFound, version.CategoryID)
		}

		if err != nil {
			return fmt.Errorf("find rule category: %w", err)
		}

		if err := tx.Omit(clause.Associations).Create(document).Error; err != nil {
			var mysqlErr *mysql.MySQLError

			if errors.As(err, &mysqlErr) && mysqlErr.Number == 1062 {
				return fmt.Errorf("%w: %w", service.ErrKnowledgeDocumentExists, err)
			}

			return fmt.Errorf("insert knowledge document: %w", err)
		}

		version.DocumentID = document.ID

		if err := tx.Omit(clause.Associations).Create(version).Error; err != nil {
			return fmt.Errorf("insert knowledge version: %w", err)
		}

		for i := range articles {
			article := &articles[i]
			article.VersionID = version.ID

			if err := tx.Omit(clause.Associations).Create(article).Error; err != nil {
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

		return nil
	})

	if err != nil {
		return fmt.Errorf("create knowledge draft transaction: %w", err)
	}

	return nil
}

func (r *KnowledgeRepository) GetDraftByID(ctx context.Context, versionID uint64) (*model.RuleVersion, []model.RuleArticle, error) {
	version, articles, _, err := r.GetDraftDetailByID(ctx, versionID)
	return version, articles, err
}

func (r *KnowledgeRepository) GetDraftDetailByID(ctx context.Context, versionID uint64) (*model.RuleVersion, []model.RuleArticle, *model.RuleVerification, error) {
	if versionID == 0 {
		return nil, nil, nil, fmt.Errorf("%w: version_id must not be zero", service.ErrInvalidArgument)
	}

	var version model.RuleVersion
	articles := make([]model.RuleArticle, 0)
	var verification *model.RuleVerification

	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		err := tx.Where("id = ? AND status = ?", versionID, model.RuleVersionStatusDraft).First(&version).Error

		if errors.Is(err, gorm.ErrRecordNotFound) {
			return service.ErrKnowledgeDraftNotFound
		}
		if err != nil {
			return fmt.Errorf("find draft version: %w", err)
		}

		if err := tx.Where("version_id = ?", version.ID).Order("article_order ASC").Find(&articles).Error; err != nil {
			return fmt.Errorf("find draft articles: %w", err)
		}

		var latest model.RuleVerification
		err = tx.Select("id", "version_id", "verified_content_revision", "verified_by", "verified_at", "verification_note", "evidence_url").Where("version_id = ?", version.ID).Order("id DESC").Take(&latest).Error

		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("find latest draft verification: %w", err)
		}

		verification = &latest
		return nil
	}, &sql.TxOptions{
		Isolation: sql.LevelRepeatableRead,
		ReadOnly:  true,
	})

	if err != nil {
		return nil, nil, nil, fmt.Errorf("get knowledge draft detail: %w", err)
	}

	return &version, articles, verification, nil
}
