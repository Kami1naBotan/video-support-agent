package repository

import (
	"context"
	"time"
	indexing "video-support-agent/internal/index"
	"video-support-agent/internal/knowledgechunk"
	"video-support-agent/internal/knowledgeworker"
	"video-support-agent/internal/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

type projectionBudget struct {
	rows  int64
	bytes int64
}

type articleProjectionRow struct {
	ArticleID      uint64
	ArticleContent string
	ChunkID        uint64
	ChunkIndex     uint32
	ChunkContent   string
}

const projectionFrom = `
FROM rule_articles AS a
LEFT JOIN knowledge_chunks AS c ON c.article_id = a.id
WHERE a.version_id = ?
`

func checkArticleProjections(
	tx *gorm.DB,
	versionID uint64,
	repair bool,
	budget *projectionBudget,
) error {
	var stats struct {
		RowCount   int64
		MaxBytes   int64
		TotalBytes int64
	}

	size := `(OCTET_LENGTH(a.content) + COALESCE(OCTET_LENGTH(c.content), 0))`
	query := `SELECT COUNT(*) AS row_count,
		COALESCE(MAX(` + size + `), 0) AS max_bytes,
		COALESCE(SUM(` + size + `), 0) AS total_bytes ` + projectionFrom

	if err := tx.Raw(query, versionID).Scan(&stats).Error; err != nil {
		return err
	}
	if stats.RowCount == 0 {
		return knowledgeworker.ErrProjectionConflict
	}
	if stats.RowCount < 0 || stats.TotalBytes < 0 ||
		stats.RowCount > int64(indexing.MaxExportDocuments)-budget.rows ||
		stats.TotalBytes > int64(indexing.MaxExportJSONBytes)-budget.bytes ||
		stats.MaxBytes > 2*int64(indexing.MaxIndexDocumentJSONBytes) {
		return knowledgeworker.ErrProjectionLimit
	}

	budget.rows += stats.RowCount
	budget.bytes += stats.TotalBytes

	var rows []articleProjectionRow
	query = `SELECT a.id AS article_id,
		a.content AS article_content,
		COALESCE(c.id, 0) AS chunk_id,
		COALESCE(c.chunk_index, 0) AS chunk_index,
		COALESCE(c.content, '') AS chunk_content ` +
		projectionFrom + ` ORDER BY a.article_order, a.id, c.chunk_index, c.id`

	if err := tx.Raw(query, versionID).Scan(&rows).Error; err != nil {
		return err
	}
	if int64(len(rows)) != stats.RowCount {
		return knowledgeworker.ErrProjectionConflict
	}

	seen := make(map[uint64]bool, len(rows))
	for _, row := range rows {
		if err := tx.Statement.Context.Err(); err != nil {
			return err
		}
		if seen[row.ArticleID] ||
			len(row.ArticleContent) > indexing.MaxIndexDocumentJSONBytes ||
			len(row.ChunkContent) > indexing.MaxIndexDocumentJSONBytes {
			return knowledgeworker.ErrProjectionConflict
		}
		seen[row.ArticleID] = true

		expected, err := knowledgechunk.FromArticle(row.ArticleID, row.ArticleContent)
		if err != nil {
			return knowledgeworker.ErrProjectionConflict
		}

		if row.ChunkID != 0 {
			if row.ChunkIndex != expected.ChunkIndex ||
				row.ChunkContent != expected.Content {
				return knowledgeworker.ErrProjectionConflict
			}
			continue
		}

		if !repair {
			return knowledgeworker.ErrProjectionConflict
		}
		if err := tx.Omit(clause.Associations).Create(&expected).Error; err != nil {
			return err
		}
	}
	return nil
}

func projectionDocuments(tx *gorm.DB) ([]model.KnowledgeDocument, error) {
	var documents []model.KnowledgeDocument
	err := tx.Select("id", "current_version_id", "publication_revision").
		Order("id").
		Limit(indexing.MaxExportDocuments + 1).
		Find(&documents).Error
	if err != nil {
		return nil, err
	}
	if len(documents) > indexing.MaxExportDocuments {
		return nil, knowledgeworker.ErrProjectionLimit
	}
	return documents, nil
}

func projectionVersion(
	tx *gorm.DB,
	document model.KnowledgeDocument,
	lock bool,
) (model.RuleVersion, error) {
	var version model.RuleVersion
	query := tx.Select("id", "document_id", "status", "withdrawn_at").
		Where("id = ? AND document_id = ?", *document.CurrentVersionID, document.ID)
	if lock {
		query = query.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	if err := query.Take(&version).Error; err != nil {
		return version, err
	}
	if version.Status != model.RuleVersionStatusPublished ||
		version.WithdrawnAt != nil {
		return version, knowledgeworker.ErrProjectionConflict
	}
	return version, nil
}

func (r *KnowledgeBuildRepository) prepareDocumentProjection(
	ctx context.Context,
	documentID uint64,
	budget *projectionBudget,
) error {
	bounded, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	db := r.db.Session(&gorm.Session{
		Logger: logger.Default.LogMode(logger.Silent),
	}).WithContext(bounded)

	return db.Transaction(func(tx *gorm.DB) error {
		var document model.KnowledgeDocument
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Select("id", "current_version_id", "publication_revision").
			Where("id = ?", documentID).Take(&document).Error; err != nil {
			return err
		}
		if document.CurrentVersionID == nil {
			return nil
		}

		version, err := projectionVersion(tx, document, true)
		if err != nil {
			return err
		}
		return checkArticleProjections(tx, version.ID, true, budget)
	})
}
