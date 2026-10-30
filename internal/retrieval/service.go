package retrieval

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type Service struct {
	searcher CandidateSearcher
	verifier CurrentVerifier
	clock    func() time.Time
}

var _ Retriever = (*Service)(nil)

func NewService(searcher CandidateSearcher, verifier CurrentVerifier) (*Service, error) {
	return NewServiceWithClock(searcher, verifier, time.Now)
}

func NewServiceWithClock(searcher CandidateSearcher, verifier CurrentVerifier, clock func() time.Time) (*Service, error) {
	if searcher == nil || verifier == nil || clock == nil {
		return nil, errors.New("retrieval searcher, verifier and clock are required")
	}

	return &Service{
		searcher: searcher,
		verifier: verifier,
		clock:    clock,
	}, nil
}

func (s *Service) Retrieve(ctx context.Context, query string, opts SearchOptions) ([]RuleChunk, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	query, opts, err := NormalizeSearch(query, opts)
	if err != nil {
		return nil, err
	}

	if s == nil || s.searcher == nil || s.verifier == nil || s.clock == nil {
		return nil, errors.New("retrieval service is not configured")
	}

	at := s.clock().UTC().Truncate(time.Millisecond)
	if at.IsZero() || at.Year() < 1 || at.Year() > 9999 {
		return nil, errors.New("retrieval clock returned an invalid time")
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	candidates, err := s.searcher.SearchCandidates(ctx, query, opts, at)
	if contextErr := ctx.Err(); contextErr != nil {
		return nil, contextErr
	}
	if err != nil {
		return nil, fmt.Errorf("retrieve candidates: %w", err)
	}

	if candidates == nil {
		return nil, fmt.Errorf("%w: searcher returned a nil candidate slice", ErrInvalidCandidates)
	}

	candidateLimit := opts.Limit * CandidateMultiplier
	if len(candidates) > candidateLimit || len(candidates) > MaxCandidates {
		return nil, fmt.Errorf("%w: searcher exceeded the candidate budget", ErrInvalidCandidates)
	}

	result := make([]RuleChunk, 0, opts.Limit)
	if len(candidates) == 0 {
		return result, nil
	}

	candidateIDs := make(map[uint64]struct{}, len(candidates))
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		id := candidate.Chunk.ChunkID
		if id == 0 {
			return nil, fmt.Errorf("%w: candidate chunk ID must not be zero", ErrInvalidCandidates)
		}
		if _, duplicate := candidateIDs[id]; duplicate {
			return nil, fmt.Errorf("%w: duplicate candidate chunk ID", ErrInvalidCandidates)
		}
		candidateIDs[id] = struct{}{}
	}

	accepted, err := s.verifier.VerifyCurrent(ctx, candidates, at)
	if contextErr := ctx.Err(); contextErr != nil {
		return nil, contextErr
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrVerificationFailed, err)
	}
	if accepted == nil {
		return nil, fmt.Errorf("%w: verifier returned a nil accepted set", ErrVerificationFailed)
	}

	for id := range accepted {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if _, exists := candidateIDs[id]; !exists {
			return nil, fmt.Errorf("%w: verifier returned an unrequested chunk ID", ErrVerificationFailed)
		}
	}

	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if _, exists := accepted[candidate.Chunk.ChunkID]; !exists {
			continue
		}

		result = append(result, copyRetrievedChunk(candidate.Chunk, opts.Highlight))
		if len(result) == opts.Limit {
			break
		}
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func copyRetrievedChunk(chunk RuleChunk, highlight bool) RuleChunk {
	result := chunk

	if chunk.ExpiresAt != nil {
		expiresAt := *chunk.ExpiresAt
		result.ExpiresAt = &expiresAt
	}

	result.Highlights = make(map[string][]string)
	if highlight {
		for field, fragments := range chunk.Highlights {
			copied := make([]string, len(fragments))
			copy(copied, fragments)
			result.Highlights[field] = copied
		}
	}

	return result
}
