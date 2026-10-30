package repository

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	indexing "video-support-agent/internal/index"
	"video-support-agent/internal/model"

	"gorm.io/gorm"
)

var _ indexing.Source = (*KnowledgeRepository)(nil)

const indexExportFrom = `
FROM knowledge_documents AS d
JOIN rule_versions AS v
  ON v.id = d.current_version_id
 AND v.document_id = d.id
LEFT JOIN rule_categories AS category
  ON category.id = v.category_id
LEFT JOIN rule_articles AS a
  ON a.version_id = v.id
LEFT JOIN knowledge_chunks AS c
  ON c.article_id = a.id
WHERE v.status = ?
  AND v.withdrawn_at IS NULL
  AND v.effective_at IS NOT NULL
  AND v.effective_at <= ?
  AND (v.expires_at IS NULL OR v.expires_at > ?)
`

const indexExportTextBytes = `(
	COALESCE(OCTET_LENGTH(v.version), 0) +
	COALESCE(OCTET_LENGTH(v.title), 0) +
	COALESCE(OCTET_LENGTH(v.source_url), 0) +
	COALESCE(OCTET_LENGTH(a.section_path), 0) +
	COALESCE(OCTET_LENGTH(a.article_number), 0) +
	COALESCE(OCTET_LENGTH(c.content), 0)
)`

type indexExportRow struct {
	DocumentID          uint64
	PublicationRevision uint64
	VersionID           uint64
	CategoryID          uint64
	ExistingCategoryID  uint64
	Version             string
	Title               string
	SourceURL           string
	Status              model.RuleVersionStatus
	EffectiveAt         *time.Time
	ExpiresAt           *time.Time
	WithdrawnAt         *time.Time
	ArticleID           uint64
	ArticleOrder        uint32
	SectionPath         string
	ArticleNumber       string
	ChunkID             uint64
	ChunkIndex          uint32
	Content             string
}

func (r *KnowledgeRepository) ExportCurrent(ctx context.Context) (*indexing.Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	snapshot := &indexing.Snapshot{
		Documents: make([]indexing.Document, 0),
	}

	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var source struct {
			DatabaseName sql.NullString
			ServerUUID   string
		}

		if err := tx.Raw(`
			SELECT DATABASE() AS database_name,
			       @@server_uuid AS server_uuid
		`).Scan(&source).Error; err != nil {
			return fmt.Errorf("read export source identity: %w", err)
		}

		if !source.DatabaseName.Valid || strings.TrimSpace(source.DatabaseName.String) == "" || strings.TrimSpace(source.ServerUUID) == "" {
			return fmt.Errorf("%w: database name and server UUID are required", indexing.ErrInvalidSource)
		}

		snapshot.SourceDatabase = source.DatabaseName.String
		snapshot.SourceServerUUID = source.ServerUUID
		snapshot.CapturedAt = r.clock().UTC().Truncate(time.Millisecond)

		if snapshot.CapturedAt.IsZero() {
			return fmt.Errorf("%w: export time is required", indexing.ErrInvalidSource)
		}

		var brokenPointer struct {
			DocumentID uint64
		}

		if err := tx.Raw(`
			SELECT d.id AS document_id
			FROM knowledge_documents AS d
			LEFT JOIN rule_versions AS v
			  ON v.id = d.current_version_id
			 AND v.document_id = d.id
			WHERE d.current_version_id IS NOT NULL
			  AND v.id IS NULL
			ORDER BY d.id
			LIMIT 1
		`).Scan(&brokenPointer).Error; err != nil {
			return fmt.Errorf("check current version references: %w", err)
		}

		if brokenPointer.DocumentID != 0 {
			return fmt.Errorf("%w: document_id=%d has an invalid current version reference", indexing.ErrInvalidSource, brokenPointer.DocumentID)
		}

		args := []any{model.RuleVersionStatusPublished, snapshot.CapturedAt, snapshot.CapturedAt}

		var stats struct {
			RowCount   int64
			MaxBytes   int64
			TotalBytes int64
		}

		statsSQL := `
			SELECT COUNT(*) AS row_count,
			       COALESCE(MAX(` + indexExportTextBytes + `), 0) AS max_bytes,
			       COALESCE(SUM(` + indexExportTextBytes + `), 0) AS total_bytes
		` + indexExportFrom

		if err := tx.Raw(statsSQL, args...).Scan(&stats).Error; err != nil {
			return fmt.Errorf("check index export size: %w", err)
		}

		if stats.RowCount > indexing.MaxExportDocuments || stats.MaxBytes > indexing.MaxIndexDocumentJSONBytes || stats.TotalBytes > indexing.MaxExportJSONBytes {
			return fmt.Errorf("%w: rows=%d max_text_bytes=%d total_text_bytes=%d", indexing.ErrExportLimitExceeded, stats.RowCount, stats.MaxBytes, stats.TotalBytes)
		}

		if stats.RowCount == 0 {
			return nil
		}

		snapshot.Documents = make([]indexing.Document, 0, int(stats.RowCount))

		selectSQL := `
			SELECT d.id AS document_id,
			       d.publication_revision,
			       v.id AS version_id,
			       v.category_id,
			       COALESCE(category.id, 0) AS existing_category_id,
			       v.version,
			       v.title,
			       v.source_url,
			       v.status,
			       v.effective_at,
			       v.expires_at,
			       v.withdrawn_at,
			       COALESCE(a.id, 0) AS article_id,
			       COALESCE(a.article_order, 0) AS article_order,
			       COALESCE(a.section_path, '') AS section_path,
			       COALESCE(a.article_number, '') AS article_number,
			       COALESCE(c.id, 0) AS chunk_id,
			       COALESCE(c.chunk_index, 0) AS chunk_index,
			       COALESCE(c.content, '') AS content
		` + indexExportFrom + `
			ORDER BY d.id, a.article_order, a.id, c.chunk_index, c.id
		`

		rows, err := tx.Raw(selectSQL, args...).Rows()
		if err != nil {
			return fmt.Errorf("open index export rows: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			if err := ctx.Err(); err != nil {
				return err
			}

			var row indexExportRow
			if err := tx.ScanRows(rows, &row); err != nil {
				return fmt.Errorf("scan index export row: %w", err)
			}

			if row.ExistingCategoryID == 0 || row.ExistingCategoryID != row.CategoryID {
				return fmt.Errorf("%w: version_id=%d has a missing category", indexing.ErrInvalidSource, row.VersionID)
			}
			if row.ArticleID == 0 || row.ChunkID == 0 {
				return fmt.Errorf("%w: document_id=%d version_id=%d article_id=%d has missing articles or chunks", indexing.ErrInvalidSource, row.DocumentID, row.VersionID, row.ArticleID)
			}

			currentVersionID := row.VersionID
			document := model.KnowledgeDocument{
				ID:                  row.DocumentID,
				CurrentVersionID:    &currentVersionID,
				PublicationRevision: row.PublicationRevision,
			}
			version := model.RuleVersion{
				ID:          row.VersionID,
				DocumentID:  row.DocumentID,
				CategoryID:  row.CategoryID,
				Version:     row.Version,
				Title:       row.Title,
				SourceURL:   row.SourceURL,
				Status:      row.Status,
				EffectiveAt: row.EffectiveAt,
				ExpiresAt:   row.ExpiresAt,
				WithdrawnAt: row.WithdrawnAt,
			}
			article := model.RuleArticle{
				ID:            row.ArticleID,
				VersionID:     row.VersionID,
				ArticleOrder:  row.ArticleOrder,
				SectionPath:   row.SectionPath,
				ArticleNumber: row.ArticleNumber,
			}
			chunk := model.KnowledgeChunk{
				ID:         row.ChunkID,
				ArticleID:  row.ArticleID,
				ChunkIndex: row.ChunkIndex,
				Content:    row.Content,
			}

			indexDocument, err := indexing.NewDocument(document, version, article, chunk, snapshot.CapturedAt)
			if err != nil {
				return fmt.Errorf("convert index export row: %w", err)
			}

			if len(snapshot.Documents) >= indexing.MaxExportDocuments {
				return fmt.Errorf("%w: too many exported documents", indexing.ErrExportLimitExceeded)
			}

			snapshot.Documents = append(snapshot.Documents, indexDocument)
		}

		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate index export rows: %w", err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("close index export rows: %w", err)
		}
		if int64(len(snapshot.Documents)) != stats.RowCount {
			return fmt.Errorf("%w: export row count changed within snapshot", indexing.ErrInvalidSource)
		}

		return ctx.Err()
	}, &sql.TxOptions{
		Isolation: sql.LevelRepeatableRead,
		ReadOnly:  true,
	})

	if err != nil {
		return nil, fmt.Errorf("export current index snapshot: %w", err)
	}

	return snapshot, nil
}
