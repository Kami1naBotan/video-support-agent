package repository

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"video-support-agent/internal/model"
	"video-support-agent/internal/retrieval"

	"gorm.io/gorm"
)

var _ retrieval.CurrentVerifier = (*KnowledgeRepository)(nil)

type searchCurrentRow struct {
	ChunkID             uint64
	ArticleID           uint64
	VersionID           uint64
	DocumentID          uint64
	CategoryID          uint64
	PublicationRevision uint64
	ChunkIndex          uint32
	ArticleOrder        uint32
	EffectiveAt         time.Time
	ExpiresAt           *time.Time
}

func (r *KnowledgeRepository) VerifyCurrent(ctx context.Context, candidates []retrieval.Candidate, at time.Time) (map[uint64]struct{}, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	at = at.UTC().Truncate(time.Millisecond)
	if at.IsZero() || at.Year() < 1 || at.Year() > 9999 {
		return nil, fmt.Errorf("%w: a valid query time is required", retrieval.ErrInvalidCandidates)
	}
	if len(candidates) > retrieval.MaxCandidates {
		return nil, fmt.Errorf("%w: candidate count exceeds the limit", retrieval.ErrInvalidCandidates)
	}

	byID := make(map[uint64]retrieval.Candidate, len(candidates))
	ids := make([]uint64, 0, len(candidates))

	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		chunk := candidate.Chunk
		if chunk.ChunkID == 0 || chunk.ArticleID == 0 || chunk.VersionID == 0 || chunk.DocumentID == 0 || chunk.CategoryID == 0 || chunk.ArticleOrder == 0 || chunk.EffectiveAt.IsZero() {
			return nil, fmt.Errorf("%w: candidate identity or metadata is incomplete", retrieval.ErrInvalidCandidates)
		}
		if _, exists := byID[chunk.ChunkID]; exists {
			return nil, fmt.Errorf("%w: duplicate chunk ID", retrieval.ErrInvalidCandidates)
		}

		byID[chunk.ChunkID] = candidate
		ids = append(ids, chunk.ChunkID)
	}

	accepted := make(map[uint64]struct{}, len(candidates))
	if len(ids) == 0 {
		return accepted, nil
	}

	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		rows, err := tx.Raw(`
			SELECT
				c.id AS chunk_id,
				a.id AS article_id,
				v.id AS version_id,
				d.id AS document_id,
				category.id AS category_id,
				d.publication_revision,
				c.chunk_index,
				a.article_order,
				v.effective_at,
				v.expires_at
			FROM knowledge_chunks AS c
			JOIN rule_articles AS a
			  ON a.id = c.article_id
			JOIN rule_versions AS v
			  ON v.id = a.version_id
			JOIN knowledge_documents AS d
			  ON d.id = v.document_id
			 AND d.current_version_id = v.id
			JOIN rule_categories AS category
			  ON category.id = v.category_id
			WHERE c.id IN ?
			  AND v.status = ?
			  AND v.withdrawn_at IS NULL
			  AND v.effective_at IS NOT NULL
			  AND v.effective_at <= ?
			  AND (v.expires_at IS NULL OR v.expires_at > ?)
			ORDER BY c.id
		`, ids, model.RuleVersionStatusPublished, at, at).Rows()
		if err != nil {
			return fmt.Errorf("open current-state rows: %w", err)
		}
		defer rows.Close()

		seen := make(map[uint64]struct{}, len(ids))

		for rows.Next() {
			if err := ctx.Err(); err != nil {
				return err
			}

			var row searchCurrentRow
			if err := tx.ScanRows(rows, &row); err != nil {
				return fmt.Errorf("scan current-state row: %w", err)
			}

			candidate, exists := byID[row.ChunkID]
			if !exists {
				return fmt.Errorf("current-state query returned an unrequested chunk")
			}
			if _, duplicate := seen[row.ChunkID]; duplicate {
				return fmt.Errorf("current-state query returned a duplicate chunk")
			}
			seen[row.ChunkID] = struct{}{}

			if !matchesCurrentCandidate(candidate, row) {
				continue
			}

			accepted[row.ChunkID] = struct{}{}
		}

		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate current-state rows: %w", err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("close current-state rows: %w", err)
		}

		return ctx.Err()
	}, &sql.TxOptions{
		Isolation: sql.LevelRepeatableRead,
		ReadOnly:  true,
	})

	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, contextErr
		}
		return nil, fmt.Errorf("%w: %w", retrieval.ErrVerificationFailed, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	return accepted, nil
}

func matchesCurrentCandidate(candidate retrieval.Candidate, row searchCurrentRow) bool {
	chunk := candidate.Chunk

	if chunk.ChunkID != row.ChunkID || chunk.ArticleID != row.ArticleID || chunk.VersionID != row.VersionID || chunk.DocumentID != row.DocumentID || chunk.CategoryID != row.CategoryID || candidate.PublicationRevision != row.PublicationRevision || chunk.ChunkIndex != row.ChunkIndex || chunk.ArticleOrder != row.ArticleOrder {
		return false
	}

	effectiveAt := row.EffectiveAt.UTC().Truncate(time.Millisecond)
	if !chunk.EffectiveAt.Equal(effectiveAt) {
		return false
	}

	if chunk.ExpiresAt == nil || row.ExpiresAt == nil {
		return chunk.ExpiresAt == nil && row.ExpiresAt == nil
	}

	expiresAt := row.ExpiresAt.UTC().Truncate(time.Millisecond)
	return chunk.ExpiresAt.Equal(expiresAt)
}
