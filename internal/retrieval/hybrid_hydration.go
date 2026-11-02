package retrieval

import (
	"context"
	"fmt"
	"time"
)

type hybridArticleKey struct {
	DocumentID uint64
	VersionID  uint64
	ArticleID  uint64
}

type hybridArticleGroup struct {
	index               int
	publicationRevision uint64
}

func PrepareHybridHydration(ctx context.Context, fused []FusedCandidate, opts HybridOptions, at time.Time) ([]Candidate, error) {
	frozen, err := freezeFusedCandidates(ctx, fused, opts, at)
	if err != nil {
		return nil, err
	}

	result := make([]Candidate, 0, len(frozen))
	for _, item := range frozen {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		candidate := item.Candidate
		candidate.Chunk = copyRetrievedChunk(candidate.Chunk, false)
		candidate.Chunk.Score = item.RRFScore
		result = append(result, candidate)
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func AssembleHybridItems(ctx context.Context, fused []FusedCandidate, hydrated map[uint64]HydratedChunk, opts HybridOptions, at time.Time) ([]HybridItem, error) {
	frozen, err := freezeFusedCandidates(ctx, fused, opts, at)
	if err != nil {
		return nil, err
	}

	if hydrated == nil || len(hydrated) > len(frozen) {
		return nil, fmt.Errorf("%w: invalid hybrid hydration map", ErrHydrationFailed)
	}

	requested := make(map[uint64]Candidate, len(frozen))
	for _, item := range frozen {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		requested[item.Candidate.Chunk.ChunkID] = item.Candidate
	}

	totalBytes := 0
	for id, item := range hydrated {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		candidate, exists := requested[id]
		if !exists || item.Chunk.ChunkID != id {
			return nil, fmt.Errorf("%w: unrequested or mismatched hydrated chunk", ErrHydrationFailed)
		}

		rowBytes, err := validateHydratedItem(item, candidate)
		if err != nil {
			return nil, err
		}
		if rowBytes > MaxHydrationTotalBytes-totalBytes {
			return nil, fmt.Errorf("%w: %w", ErrHydrationFailed, ErrHydrationLimit)
		}
		totalBytes += rowBytes
	}

	items := make([]HybridItem, 0, len(hydrated))
	groups := make(map[hybridArticleKey]hybridArticleGroup)

	for _, fusedItem := range frozen {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		id := fusedItem.Candidate.Chunk.ChunkID
		rule, exists := hydrated[id]
		if !exists {
			continue
		}

		key := hybridArticleKey{
			DocumentID: rule.Chunk.DocumentID,
			VersionID:  rule.Chunk.VersionID,
			ArticleID:  rule.Chunk.ArticleID,
		}

		group, exists := groups[key]
		if exists {
			current := &items[group.index]

			if group.publicationRevision != fusedItem.Candidate.PublicationRevision || !sameHybridArticle(current.Rule, rule) {
				return nil, fmt.Errorf("%w: inconsistent hydrated article", ErrHydrationFailed)
			}

			current.ChunkIDs = append(current.ChunkIDs, id)
			current.Matches = append(current.Matches, fusedItem.Matches...)
			continue
		}

		rule.Chunk = copyRetrievedChunk(rule.Chunk, false)
		rule.Chunk.Score = fusedItem.RRFScore

		matches := make([]SourceMatch, len(fusedItem.Matches))
		copy(matches, fusedItem.Matches)

		groups[key] = hybridArticleGroup{
			index:               len(items),
			publicationRevision: fusedItem.Candidate.PublicationRevision,
		}
		items = append(items, HybridItem{
			Rule:     rule,
			ChunkIDs: []uint64{id},
			RRFScore: fusedItem.RRFScore,
			Matches:  matches,
		})
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

func sameHybridArticle(left, right HydratedChunk) bool {
	a, b := left.Chunk, right.Chunk

	if a.DocumentID != b.DocumentID ||
		a.VersionID != b.VersionID ||
		a.ArticleID != b.ArticleID ||
		a.CategoryID != b.CategoryID ||
		a.Version != b.Version ||
		a.Title != b.Title ||
		a.SourceURL != b.SourceURL ||
		a.SectionPath != b.SectionPath ||
		a.ArticleNumber != b.ArticleNumber ||
		a.ArticleOrder != b.ArticleOrder ||
		!a.EffectiveAt.Equal(b.EffectiveAt) ||
		left.ArticleContent != right.ArticleContent {
		return false
	}

	if a.ExpiresAt == nil || b.ExpiresAt == nil {
		return a.ExpiresAt == nil && b.ExpiresAt == nil
	}
	return a.ExpiresAt.Equal(*b.ExpiresAt)
}
