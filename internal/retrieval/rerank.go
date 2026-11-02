package retrieval

import (
	"context"
	"errors"
	"fmt"
	"math"
)

var ErrInvalidRerank = errors.New("invalid rerank result")

type RerankDocument struct {
	ChunkID        uint64
	Title          string
	ArticleContent string
	RRFScore       float64
}

type Reranker interface {
	Rerank(ctx context.Context, query string, documents []RerankDocument) ([]uint64, error)
}

func BuildRerankDocuments(ctx context.Context, items []HybridItem) ([]RerankDocument, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if items == nil || len(items) > MaxCandidates {
		return nil, fmt.Errorf("%w: invalid rerank item count", ErrInvalidRerank)
	}

	documents := make([]RerankDocument, 0, len(items))
	seenIDs := make(map[uint64]struct{}, len(items))
	seenArticles := make(map[hybridArticleKey]struct{}, len(items))
	totalBytes := 0
	maxScore := 2.0 / float64(RRFSmoothing+1)

	for _, item := range items {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		chunk := item.Rule.Chunk
		if chunk.ChunkID == 0 || chunk.DocumentID == 0 || chunk.VersionID == 0 || chunk.ArticleID == 0 || chunk.CategoryID == 0 {
			return nil, fmt.Errorf("%w: invalid rerank item identity", ErrInvalidRerank)
		}

		if _, exists := seenIDs[chunk.ChunkID]; exists {
			return nil, fmt.Errorf("%w: duplicate representative chunk", ErrInvalidRerank)
		}
		seenIDs[chunk.ChunkID] = struct{}{}

		key := hybridArticleKey{
			DocumentID: chunk.DocumentID,
			VersionID:  chunk.VersionID,
			ArticleID:  chunk.ArticleID,
		}
		if _, exists := seenArticles[key]; exists {
			return nil, fmt.Errorf("%w: duplicate rerank article", ErrInvalidRerank)
		}
		seenArticles[key] = struct{}{}

		if math.IsNaN(item.RRFScore) || math.IsInf(item.RRFScore, 0) || item.RRFScore <= 0 || item.RRFScore > maxScore || chunk.Score != item.RRFScore {
			return nil, fmt.Errorf("%w: invalid rerank RRF score", ErrInvalidRerank)
		}

		rowBytes, err := validateHydratedItem(
			item.Rule,
			Candidate{Chunk: chunk},
		)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalidRerank, err)
		}
		if rowBytes > MaxHydrationTotalBytes-totalBytes {
			return nil, fmt.Errorf("%w: %w", ErrInvalidRerank, ErrHydrationLimit)
		}
		totalBytes += rowBytes

		documents = append(documents, RerankDocument{
			ChunkID:        chunk.ChunkID,
			Title:          chunk.Title,
			ArticleContent: item.Rule.ArticleContent,
			RRFScore:       item.RRFScore,
		})
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return documents, nil
}

func ApplyRerankOrder(ctx context.Context, items []HybridItem, order []uint64) ([]HybridItem, error) {
	if _, err := BuildRerankDocuments(ctx, items); err != nil {
		return nil, err
	}

	if order == nil || len(order) != len(items) {
		return nil, fmt.Errorf("%w: rerank order must include every item", ErrInvalidRerank)
	}

	byID := make(map[uint64]HybridItem, len(items))
	for _, item := range items {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		byID[item.Rule.Chunk.ChunkID] = item
	}

	result := make([]HybridItem, 0, len(items))
	seen := make(map[uint64]struct{}, len(order))

	for _, id := range order {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		item, exists := byID[id]
		if !exists {
			return nil, fmt.Errorf("%w: unknown representative chunk", ErrInvalidRerank)
		}
		if _, exists := seen[id]; exists {
			return nil, fmt.Errorf("%w: repeated representative chunk", ErrInvalidRerank)
		}
		seen[id] = struct{}{}

		item.Rule.Chunk = copyRetrievedChunk(item.Rule.Chunk, true)

		chunkIDs := make([]uint64, len(item.ChunkIDs))
		copy(chunkIDs, item.ChunkIDs)
		item.ChunkIDs = chunkIDs

		matches := make([]SourceMatch, len(item.Matches))
		copy(matches, item.Matches)
		item.Matches = matches

		result = append(result, item)
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}
