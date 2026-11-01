package milvus

import (
	"context"
	"fmt"
	"time"

	"video-support-agent/internal/embedding"
	"video-support-agent/internal/retrieval"
)

type QueryEmbedder interface {
	EmbedWithIdentity(ctx context.Context, texts []string) (embedding.BatchResult, error)
}

var _ QueryEmbedder = (*embedding.Client)(nil)

type PreparedQuery struct {
	Options        retrieval.SearchOptions
	At             time.Time
	CandidateLimit int
	Identity       embedding.ModelIdentity
	Vector         []float32
}

func PrepareQuery(ctx context.Context, query string, opts retrieval.SearchOptions, at time.Time, embedder QueryEmbedder) (*PreparedQuery, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, ok := ctx.Deadline(); !ok {
		return nil, fmt.Errorf("%w: query preparation requires an overall deadline", ErrInput)
	}
	if embedder == nil {
		return nil, fmt.Errorf("%w: query embedder is required", ErrInput)
	}

	if len(query) > embedding.MaxTextBytes {
		return nil, fmt.Errorf("%w: query exceeds the byte budget", retrieval.ErrInvalidQuery)
	}

	if opts.Section != "" || opts.ArticleNumber != "" || opts.Highlight {
		return nil, fmt.Errorf("%w: vector search does not support section, article_number or highlight", retrieval.ErrInvalidQuery)
	}

	normalized, options, err := retrieval.NormalizeSearch(query, opts)
	if err != nil {
		return nil, err
	}
	if normalized == "" {
		return nil, fmt.Errorf("%w: vector search requires a non-empty query", retrieval.ErrInvalidQuery)
	}

	if at.IsZero() || at.Year() < 1 || at.Year() > 9999 {
		return nil, fmt.Errorf("%w: invalid query time", ErrInput)
	}
	queryTime := at.UTC().Truncate(time.Millisecond)

	candidateLimit := options.Limit * retrieval.CandidateMultiplier
	if candidateLimit < 1 || candidateLimit > retrieval.MaxCandidates {
		return nil, fmt.Errorf("%w: invalid candidate budget", ErrInput)
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	batch, err := embedder.EmbedWithIdentity(ctx, []string{normalized})
	if contextErr := ctx.Err(); contextErr != nil {
		return nil, contextErr
	}
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}

	if err := checkIdentity(batch.Identity, batch.Identity.Digest); err != nil {
		return nil, err
	}

	if len(batch.Vectors) != 1 {
		return nil, fmt.Errorf("%w: query embedding must return exactly one vector", embedding.ErrResponse)
	}
	if len(batch.Vectors[0]) != embedding.Dimensions {
		return nil, fmt.Errorf("%w: query vector dimensions differ", embedding.ErrResponse)
	}

	raw := make([]float64, embedding.Dimensions)
	for i, value := range batch.Vectors[0] {
		raw[i] = float64(value)
	}
	vectors, err := embedding.ValidateVectors([][]float64{raw}, 1)
	if err != nil {
		return nil, err
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &PreparedQuery{
		Options:        options,
		At:             queryTime,
		CandidateLimit: candidateLimit,
		Identity:       batch.Identity,
		Vector:         vectors[0],
	}, nil
}
