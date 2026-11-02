package retrieval

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"
)

type HydratedRetriever interface {
	Retrieve(ctx context.Context, query string, opts SearchOptions) ([]HydratedChunk, error)
}

type MilvusRetriever struct {
	searcher CandidateSearcher
	hydrator CurrentHydrator
	timeout  time.Duration
	clock    func() time.Time
}

var _ HydratedRetriever = (*MilvusRetriever)(nil)

func NewMilvusRetriever(searcher CandidateSearcher, hydrator CurrentHydrator, timeout time.Duration) (*MilvusRetriever, error) {
	return NewMilvusRetrieverWithClock(searcher, hydrator, timeout, time.Now)
}

func NewMilvusRetrieverWithClock(searcher CandidateSearcher, hydrator CurrentHydrator, timeout time.Duration, clock func() time.Time) (*MilvusRetriever, error) {
	if searcher == nil || hydrator == nil || clock == nil {
		return nil, errors.New("vector searcher, hydrator and clock are required")
	}
	if timeout <= 0 {
		return nil, errors.New("vector retrieval timeout must be positive")
	}
	return &MilvusRetriever{
		searcher: searcher,
		hydrator: hydrator,
		timeout:  timeout,
		clock:    clock,
	}, nil
}

func (r *MilvusRetriever) Retrieve(ctx context.Context, query string, opts SearchOptions) ([]HydratedChunk, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r == nil || r.searcher == nil || r.hydrator == nil ||
		r.clock == nil || r.timeout <= 0 {
		return nil, errors.New("vector retriever is not configured")
	}

	if len(query) > 64<<10 {
		return nil, fmt.Errorf("%w: query exceeds byte budget", ErrInvalidQuery)
	}
	if opts.Section != "" || opts.ArticleNumber != "" || opts.Highlight {
		return nil, fmt.Errorf("%w: vector search does not support section, article_number or highlight", ErrInvalidQuery)
	}
	query, opts, err := NormalizeSearch(query, opts)
	if err != nil {
		return nil, err
	}
	if query == "" {
		return nil, fmt.Errorf("%w: vector query is required", ErrInvalidQuery)
	}

	opCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	at := r.clock().UTC().Truncate(time.Millisecond)
	if at.IsZero() || at.Year() < 1 || at.Year() > 9999 {
		return nil, errors.New("vector retrieval clock returned invalid time")
	}

	candidates, err := r.searcher.SearchCandidates(opCtx, query, opts, at)
	if contextErr := opCtx.Err(); contextErr != nil {
		return nil, contextErr
	}
	if err != nil {
		return nil, fmt.Errorf("retrieve vector candidates: %w", err)
	}

	frozen, err := freezeVectorCandidates(candidates, opts, at)
	if err != nil {
		return nil, err
	}
	result := make([]HydratedChunk, 0, opts.Limit)
	if len(frozen) == 0 {
		return result, nil
	}

	byID := make(map[uint64]Candidate, len(frozen))
	hydrationInput := make([]Candidate, len(frozen))
	for i, candidate := range frozen {
		byID[candidate.Chunk.ChunkID] = candidate

		hydrationInput[i] = Candidate{
			Chunk:               copyRetrievedChunk(candidate.Chunk, false),
			PublicationRevision: candidate.PublicationRevision,
		}
	}

	hydrated, err := r.hydrator.HydrateCurrent(opCtx, hydrationInput, at)
	if contextErr := opCtx.Err(); contextErr != nil {
		return nil, contextErr
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrHydrationFailed, err)
	}
	if hydrated == nil || len(hydrated) > len(frozen) {
		return nil, fmt.Errorf("%w: invalid hydrated result map", ErrHydrationFailed)
	}

	totalBytes := 0
	for id, item := range hydrated {
		if err := opCtx.Err(); err != nil {
			return nil, err
		}
		candidate, exists := byID[id]
		if !exists || item.Chunk.ChunkID != id {
			return nil, fmt.Errorf("%w: hydration returned an unrequested or mismatched ID", ErrHydrationFailed)
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

	for _, candidate := range frozen {
		if err := opCtx.Err(); err != nil {
			return nil, err
		}
		item, exists := hydrated[candidate.Chunk.ChunkID]
		if !exists {
			continue
		}

		item.Chunk = copyRetrievedChunk(item.Chunk, false)
		item.Chunk.Score = candidate.Chunk.Score
		result = append(result, item)
		if len(result) == opts.Limit {
			break
		}
	}

	if err := opCtx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func freezeVectorCandidates(candidates []Candidate, opts SearchOptions, at time.Time) ([]Candidate, error) {
	if candidates == nil || len(candidates) > opts.Limit*CandidateMultiplier || len(candidates) > MaxCandidates {
		return nil, fmt.Errorf("%w: invalid vector candidate count", ErrInvalidCandidates)
	}

	result := make([]Candidate, 0, len(candidates))
	seen := make(map[uint64]struct{}, len(candidates))
	previousScore := math.Inf(1)

	for _, candidate := range candidates {
		chunk := candidate.Chunk
		if chunk.ChunkID == 0 || chunk.DocumentID == 0 ||
			chunk.VersionID == 0 || chunk.ArticleID == 0 ||
			chunk.CategoryID == 0 ||
			!utf8.ValidString(chunk.Version) ||
			strings.TrimSpace(chunk.Version) == "" ||
			len(chunk.Version) > 256 ||
			chunk.EffectiveAt.IsZero() ||
			chunk.EffectiveAt.Year() < 1 || chunk.EffectiveAt.Year() > 9999 ||
			chunk.EffectiveAt.After(at) ||
			math.IsNaN(chunk.Score) || math.IsInf(chunk.Score, 0) ||
			chunk.Score < -1 || chunk.Score > 1 ||
			chunk.Score > previousScore+1e-6 {
			return nil, fmt.Errorf("%w: invalid vector candidate", ErrInvalidCandidates)
		}
		if chunk.ExpiresAt != nil {
			if chunk.ExpiresAt.Year() < 1 || chunk.ExpiresAt.Year() > 9999 || !chunk.ExpiresAt.After(chunk.EffectiveAt) || !chunk.ExpiresAt.After(at) {
				return nil, fmt.Errorf("%w: invalid candidate expiration", ErrInvalidCandidates)
			}
		}
		if (opts.DocumentID != 0 && chunk.DocumentID != opts.DocumentID) ||
			(opts.VersionID != 0 && chunk.VersionID != opts.VersionID) ||
			(opts.CategoryID != 0 && chunk.CategoryID != opts.CategoryID) {
			return nil, fmt.Errorf("%w: candidate violates filters", ErrInvalidCandidates)
		}
		if _, exists := seen[chunk.ChunkID]; exists {
			return nil, fmt.Errorf("%w: duplicate vector candidate", ErrInvalidCandidates)
		}
		seen[chunk.ChunkID] = struct{}{}
		previousScore = chunk.Score

		result = append(result, Candidate{
			Chunk:               copyRetrievedChunk(chunk, false),
			PublicationRevision: candidate.PublicationRevision,
		})
	}
	return result, nil
}

func validateHydratedItem(item HydratedChunk, candidate Candidate) (int, error) {
	actual, expected := item.Chunk, candidate.Chunk

	if actual.ChunkID != expected.ChunkID ||
		actual.DocumentID != expected.DocumentID ||
		actual.VersionID != expected.VersionID ||
		actual.ArticleID != expected.ArticleID ||
		actual.CategoryID != expected.CategoryID ||
		actual.Version != expected.Version ||
		!actual.EffectiveAt.Equal(expected.EffectiveAt) ||
		actual.ArticleOrder == 0 {
		return 0, fmt.Errorf("%w: hydrated metadata differs", ErrHydrationFailed)
	}
	if actual.ExpiresAt == nil || expected.ExpiresAt == nil {
		if actual.ExpiresAt != nil || expected.ExpiresAt != nil {
			return 0, fmt.Errorf("%w: hydrated expiration differs", ErrHydrationFailed)
		}
	} else if !actual.ExpiresAt.Equal(*expected.ExpiresAt) {
		return 0, fmt.Errorf("%w: hydrated expiration differs", ErrHydrationFailed)
	}

	rowBytes := 0
	for _, text := range []string{
		actual.Version, actual.Title, actual.SourceURL,
		actual.SectionPath, actual.ArticleNumber,
		actual.Content, item.ArticleContent,
	} {
		if !utf8.ValidString(text) {
			return 0, fmt.Errorf("%w: invalid hydrated UTF-8", ErrHydrationFailed)
		}
		if len(text) > MaxHydrationRowBytes-rowBytes {
			return 0, fmt.Errorf("%w: %w", ErrHydrationFailed, ErrHydrationLimit)
		}
		rowBytes += len(text)
	}
	if strings.TrimSpace(actual.Content) == "" || strings.TrimSpace(item.ArticleContent) == "" {
		return 0, fmt.Errorf("%w: hydrated content is empty", ErrHydrationFailed)
	}
	return rowBytes, nil
}
