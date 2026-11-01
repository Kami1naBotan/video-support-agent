package milvus

import (
	"context"
	"fmt"
	"time"

	"video-support-agent/internal/retrieval"
)

type Searcher struct {
	embedder QueryEmbedder
	client   *Client
	timeout  time.Duration
}

var _ retrieval.CandidateSearcher = (*Searcher)(nil)

func NewSearcher(embedder QueryEmbedder, client *Client, timeout time.Duration) (*Searcher, error) {
	if embedder == nil || client == nil || client.http == nil {
		return nil, fmt.Errorf("%w: query embedder and initialized milvus client are required", ErrInput)
	}
	if timeout <= 0 {
		return nil, fmt.Errorf("%w: candidate search timeout must be positive", ErrInput)
	}
	return &Searcher{
		embedder: embedder,
		client:   client,
		timeout:  timeout,
	}, nil
}

func (s *Searcher) SearchCandidates(ctx context.Context, query string, opts retrieval.SearchOptions, at time.Time) ([]retrieval.Candidate, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s == nil || s.embedder == nil || s.client == nil || s.client.http == nil || s.timeout <= 0 {
		return nil, fmt.Errorf("%w: candidate searcher is not configured", ErrInput)
	}

	opCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	prepared, err := PrepareQuery(
		opCtx, query, opts, at, s.embedder,
	)
	if contextErr := opCtx.Err(); contextErr != nil {
		return nil, contextErr
	}
	if err != nil {
		return nil, fmt.Errorf("prepare vector query: %w", err)
	}

	hits, err := s.client.SearchPrepared(opCtx, prepared)
	if contextErr := opCtx.Err(); contextErr != nil {
		return nil, contextErr
	}
	if err != nil {
		return nil, fmt.Errorf("search vector candidates: %w", err)
	}
	if hits == nil || len(hits) > prepared.CandidateLimit {
		return nil, fmt.Errorf("%w: invalid vector candidate list", retrieval.ErrInvalidCandidates)
	}

	candidates := make([]retrieval.Candidate, 0, len(hits))
	for _, hit := range hits {
		if err := opCtx.Err(); err != nil {
			return nil, err
		}

		var expiresAt *time.Time
		if hit.ExpiresAt != nil {
			value := *hit.ExpiresAt
			expiresAt = &value
		}

		candidates = append(candidates, retrieval.Candidate{
			PublicationRevision: hit.PublicationRevision,
			Chunk: retrieval.RuleChunk{
				ChunkID:     hit.ChunkID,
				DocumentID:  hit.DocumentID,
				VersionID:   hit.VersionID,
				ArticleID:   hit.ArticleID,
				CategoryID:  hit.CategoryID,
				Version:     hit.Version,
				EffectiveAt: hit.EffectiveAt,
				ExpiresAt:   expiresAt,
				Score:       hit.Score,
				Highlights:  make(map[string][]string),
			},
		})
	}

	if err := opCtx.Err(); err != nil {
		return nil, err
	}
	return candidates, nil
}
