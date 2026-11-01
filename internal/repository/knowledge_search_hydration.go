package repository

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"video-support-agent/internal/model"
	"video-support-agent/internal/retrieval"

	"gorm.io/gorm"
)

var _ retrieval.CurrentHydrator = (*KnowledgeRepository)(nil)

const hydrationFrom = `
FROM knowledge_chunks AS c
JOIN rule_articles AS a ON a.id = c.article_id
JOIN rule_versions AS v ON v.id = a.version_id
JOIN knowledge_documents AS d
  ON d.id = v.document_id AND d.current_version_id = v.id
JOIN rule_categories AS category ON category.id = v.category_id
WHERE c.id IN ?
  AND v.status = ?
  AND v.withdrawn_at IS NULL
  AND v.effective_at IS NOT NULL
  AND v.effective_at <= ?
  AND (v.expires_at IS NULL OR v.expires_at > ?)
`

const hydrationTextBytes = `(
	COALESCE(OCTET_LENGTH(v.version), 0) +
	COALESCE(OCTET_LENGTH(v.title), 0) +
	COALESCE(OCTET_LENGTH(v.source_url), 0) +
	COALESCE(OCTET_LENGTH(a.section_path), 0) +
	COALESCE(OCTET_LENGTH(a.article_number), 0) +
	COALESCE(OCTET_LENGTH(c.content), 0) +
	COALESCE(OCTET_LENGTH(a.content), 0)
)`

type hydrationRow struct {
	ChunkID             uint64
	ArticleID           uint64
	VersionID           uint64
	DocumentID          uint64
	CategoryID          uint64
	PublicationRevision uint64
	ChunkIndex          uint32
	ArticleOrder        uint32
	Version             string
	Title               string
	SourceURL           string
	SectionPath         string
	ArticleNumber       string
	Content             string
	ArticleContent      string
	EffectiveAt         time.Time
	ExpiresAt           *time.Time
}

func (r *KnowledgeRepository) HydrateCurrent(ctx context.Context, candidates []retrieval.Candidate, at time.Time) (map[uint64]retrieval.HydratedChunk, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, ok := ctx.Deadline(); !ok {
		return nil, fmt.Errorf("%w: hydration requires an overall deadline", retrieval.ErrInvalidCandidates)
	}
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("%w: repository is not configured", retrieval.ErrHydrationFailed)
	}
	at = at.UTC().Truncate(time.Millisecond)
	if at.IsZero() || at.Year() < 1 || at.Year() > 9999 {
		return nil, fmt.Errorf("%w: invalid hydration time", retrieval.ErrInvalidCandidates)
	}
	if candidates == nil || len(candidates) > retrieval.MaxCandidates {
		return nil, fmt.Errorf("%w: invalid hydration candidate list", retrieval.ErrInvalidCandidates)
	}

	byID := make(map[uint64]retrieval.Candidate, len(candidates))
	ids := make([]uint64, 0, len(candidates))
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		chunk := candidate.Chunk
		if chunk.ChunkID == 0 || chunk.ArticleID == 0 ||
			chunk.VersionID == 0 || chunk.DocumentID == 0 ||
			chunk.CategoryID == 0 ||
			chunk.EffectiveAt.IsZero() ||
			chunk.EffectiveAt.Year() < 1 || chunk.EffectiveAt.Year() > 9999 ||
			chunk.EffectiveAt.After(at) ||
			!utf8.ValidString(chunk.Version) ||
			strings.TrimSpace(chunk.Version) == "" ||
			len(chunk.Version) > 256 ||
			math.IsNaN(chunk.Score) || math.IsInf(chunk.Score, 0) ||
			chunk.Score < -1 || chunk.Score > 1 {
			return nil, fmt.Errorf("%w: invalid hydration candidate", retrieval.ErrInvalidCandidates)
		}
		if chunk.ExpiresAt != nil {
			expires := *chunk.ExpiresAt
			if expires.Year() < 1 || expires.Year() > 9999 || !expires.After(chunk.EffectiveAt) || !expires.After(at) {
				return nil, fmt.Errorf("%w: invalid candidate expiration", retrieval.ErrInvalidCandidates)
			}
			chunk.ExpiresAt = &expires
		}
		if _, exists := byID[chunk.ChunkID]; exists {
			return nil, fmt.Errorf("%w: duplicate hydration ID", retrieval.ErrInvalidCandidates)
		}
		candidate.Chunk = chunk
		byID[chunk.ChunkID] = candidate
		ids = append(ids, chunk.ChunkID)
	}

	result := make(map[uint64]retrieval.HydratedChunk, len(ids))
	if len(ids) == 0 {
		return result, nil
	}

	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		args := []any{
			ids, model.RuleVersionStatusPublished, at, at,
		}

		var stats struct {
			RowCount   int64
			MaxBytes   int64
			TotalBytes int64
		}
		statsSQL := `
SELECT COUNT(*) AS row_count,
       COALESCE(MAX(` + hydrationTextBytes + `), 0) AS max_bytes,
       COALESCE(SUM(` + hydrationTextBytes + `), 0) AS total_bytes
` + hydrationFrom

		if err := tx.Raw(statsSQL, args...).Scan(&stats).Error; err != nil {
			return fmt.Errorf("read hydration budget: %w", err)
		}
		if stats.RowCount < 0 || stats.RowCount > int64(len(ids)) {
			return fmt.Errorf("invalid hydration row count")
		}
		if stats.MaxBytes > retrieval.MaxHydrationRowBytes || stats.TotalBytes > retrieval.MaxHydrationTotalBytes {
			return retrieval.ErrHydrationLimit
		}
		if stats.RowCount == 0 {
			return ctx.Err()
		}

		var rows []hydrationRow
		err := tx.Raw(`
SELECT
    c.id AS chunk_id,
    a.id AS article_id,
    v.id AS version_id,
    d.id AS document_id,
    category.id AS category_id,
    d.publication_revision,
    c.chunk_index,
    a.article_order,
    v.version,
    v.title,
    v.source_url,
    a.section_path,
    a.article_number,
    c.content,
    a.content AS article_content,
    v.effective_at,
    v.expires_at
`+hydrationFrom+` ORDER BY c.id`, args...).Scan(&rows).Error
		if err != nil {
			return fmt.Errorf("read hydration rows: %w", err)
		}
		if int64(len(rows)) != stats.RowCount {
			return fmt.Errorf("hydration snapshot row count differs")
		}

		seen := make(map[uint64]struct{}, len(rows))
		for _, row := range rows {
			if err := ctx.Err(); err != nil {
				return err
			}
			candidate, exists := byID[row.ChunkID]
			if !exists {
				return fmt.Errorf("hydration returned an unrequested ID")
			}
			if _, duplicate := seen[row.ChunkID]; duplicate {
				return fmt.Errorf("hydration returned a duplicate ID")
			}
			seen[row.ChunkID] = struct{}{}

			if !matchesHydrationCandidate(candidate, row) {
				continue
			}
			if row.ArticleOrder == 0 {
				return fmt.Errorf("hydration returned invalid article order")
			}
			for _, value := range []string{
				row.Version, row.Title, row.SourceURL,
				row.SectionPath, row.ArticleNumber,
				row.Content, row.ArticleContent,
			} {
				if !utf8.ValidString(value) {
					return fmt.Errorf("hydration returned invalid UTF-8")
				}
			}
			if strings.TrimSpace(row.Content) == "" || strings.TrimSpace(row.ArticleContent) == "" {
				return fmt.Errorf("hydration returned empty rule content")
			}

			var expiresAt *time.Time
			if row.ExpiresAt != nil {
				value := row.ExpiresAt.UTC().Truncate(time.Millisecond)
				expiresAt = &value
			}

			result[row.ChunkID] = retrieval.HydratedChunk{
				Chunk: retrieval.RuleChunk{
					ChunkID:       row.ChunkID,
					DocumentID:    row.DocumentID,
					VersionID:     row.VersionID,
					ArticleID:     row.ArticleID,
					CategoryID:    row.CategoryID,
					ChunkIndex:    row.ChunkIndex,
					ArticleOrder:  row.ArticleOrder,
					Version:       row.Version,
					Title:         row.Title,
					SectionPath:   row.SectionPath,
					ArticleNumber: row.ArticleNumber,
					Content:       row.Content,
					SourceURL:     row.SourceURL,
					EffectiveAt:   row.EffectiveAt.UTC().Truncate(time.Millisecond),
					ExpiresAt:     expiresAt,
					Highlights:    make(map[string][]string),
				},
				ArticleContent: row.ArticleContent,
			}
		}
		return ctx.Err()
	}, &sql.TxOptions{
		Isolation: sql.LevelRepeatableRead,
		ReadOnly:  true,
	})

	if contextErr := ctx.Err(); contextErr != nil {
		return nil, contextErr
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %w", retrieval.ErrHydrationFailed, err)
	}
	return result, nil
}

func matchesHydrationCandidate(candidate retrieval.Candidate, row hydrationRow) bool {
	chunk := candidate.Chunk
	if chunk.ChunkID != row.ChunkID ||
		chunk.ArticleID != row.ArticleID ||
		chunk.VersionID != row.VersionID ||
		chunk.DocumentID != row.DocumentID ||
		chunk.CategoryID != row.CategoryID ||
		candidate.PublicationRevision != row.PublicationRevision ||
		chunk.Version != row.Version {
		return false
	}
	if !chunk.EffectiveAt.Equal(
		row.EffectiveAt.UTC().Truncate(time.Millisecond),
	) {
		return false
	}
	if chunk.ExpiresAt == nil || row.ExpiresAt == nil {
		return chunk.ExpiresAt == nil && row.ExpiresAt == nil
	}
	return chunk.ExpiresAt.Equal(
		row.ExpiresAt.UTC().Truncate(time.Millisecond),
	)
}
