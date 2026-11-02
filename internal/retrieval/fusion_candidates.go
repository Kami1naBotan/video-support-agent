package retrieval

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"
)

type RankedCandidates struct {
	Source     RetrievalSource
	Candidates []Candidate
}

func FreezeRankedCandidates(ctx context.Context, list RankedCandidates, opts HybridOptions, at time.Time) (RankedCandidates, error) {
	if err := ctx.Err(); err != nil {
		return RankedCandidates{}, err
	}

	opts, at, err := normalizeFusionOptions(opts, at)
	if err != nil {
		return RankedCandidates{}, err
	}

	if list.Source != SourceES && list.Source != SourceMilvus {
		return RankedCandidates{}, fmt.Errorf("%w: unknown retrieval source", ErrInvalidCandidates)
	}

	if list.Candidates == nil {
		return RankedCandidates{}, fmt.Errorf("%w: successful source returned a nil candidate slice", ErrInvalidCandidates)
	}

	limit := opts.TopK * CandidateMultiplier
	if len(list.Candidates) > limit || len(list.Candidates) > MaxCandidates {
		return RankedCandidates{}, fmt.Errorf("%w: source exceeded candidate budget", ErrInvalidCandidates)
	}

	result := RankedCandidates{
		Source:     list.Source,
		Candidates: make([]Candidate, 0, len(list.Candidates)),
	}

	previousScore := math.Inf(1)
	orderTolerance := 0.0
	if list.Source == SourceMilvus {
		orderTolerance = 1e-6
	}

	for i, candidate := range list.Candidates {
		if err := ctx.Err(); err != nil {
			return RankedCandidates{}, err
		}

		frozen, err := freezeFusionCandidate(candidate, list.Source, opts, at)
		if err != nil {
			return RankedCandidates{}, err
		}

		if i > 0 && frozen.Chunk.Score > previousScore+orderTolerance {
			return RankedCandidates{}, fmt.Errorf("%w: source scores are out of order", ErrInvalidCandidates)
		}

		previousScore = frozen.Chunk.Score
		result.Candidates = append(result.Candidates, frozen)
	}

	if err := ctx.Err(); err != nil {
		return RankedCandidates{}, err
	}

	return result, nil
}

func normalizeFusionOptions(opts HybridOptions, at time.Time) (HybridOptions, time.Time, error) {
	if opts.TopK == 0 {
		opts.TopK = DefaultLimit
	}
	if opts.TopK < 1 || opts.TopK > MaxLimit {
		return HybridOptions{}, time.Time{}, fmt.Errorf("%w: fusion top-k is out of range", ErrInvalidQuery)
	}

	at = at.UTC().Truncate(time.Millisecond)
	if at.IsZero() || at.Year() < 1 || at.Year() > 9999 {
		return HybridOptions{}, time.Time{}, fmt.Errorf("%w: invalid fusion time", ErrInvalidCandidates)
	}

	return opts, at, nil
}

func freezeFusionCandidate(candidate Candidate, source RetrievalSource, opts HybridOptions, at time.Time) (Candidate, error) {
	chunk := candidate.Chunk

	if chunk.ChunkID == 0 || chunk.DocumentID == 0 || chunk.VersionID == 0 || chunk.ArticleID == 0 || chunk.CategoryID == 0 {
		return Candidate{}, fmt.Errorf("%w: candidate identity must not be zero", ErrInvalidCandidates)
	}

	if !utf8.ValidString(chunk.Version) || strings.TrimSpace(chunk.Version) == "" || len(chunk.Version) > 256 {
		return Candidate{}, fmt.Errorf("%w: invalid candidate version", ErrInvalidCandidates)
	}

	if math.IsNaN(chunk.Score) || math.IsInf(chunk.Score, 0) {
		return Candidate{}, fmt.Errorf("%w: candidate score must be finite", ErrInvalidCandidates)
	}

	if (source == SourceES && chunk.Score < 0) || (source == SourceMilvus && (chunk.Score < -1 || chunk.Score > 1)) {
		return Candidate{}, fmt.Errorf("%w: candidate score violates source range", ErrInvalidCandidates)
	}

	effective := chunk.EffectiveAt.UTC().Truncate(time.Millisecond)

	if effective.IsZero() || effective.Year() < 1 || effective.Year() > 9999 || effective.After(at) {
		return Candidate{}, fmt.Errorf("%w: invalid candidate effective time", ErrInvalidCandidates)
	}

	var expires *time.Time
	if chunk.ExpiresAt != nil {
		value := chunk.ExpiresAt.UTC().Truncate(time.Millisecond)

		if value.Year() < 1 || value.Year() > 9999 || !value.After(effective) || !value.After(at) {
			return Candidate{}, fmt.Errorf("%w: invalid candidate expiration", ErrInvalidCandidates)
		}

		expires = &value
	}

	if (opts.DocumentID != 0 && chunk.DocumentID != opts.DocumentID) ||
		(opts.VersionID != 0 && chunk.VersionID != opts.VersionID) ||
		(opts.CategoryID != 0 && chunk.CategoryID != opts.CategoryID) {
		return Candidate{}, fmt.Errorf("%w: candidate violates requested filters", ErrInvalidCandidates)
	}

	return Candidate{
		PublicationRevision: candidate.PublicationRevision,
		Chunk: RuleChunk{
			ChunkID:     chunk.ChunkID,
			DocumentID:  chunk.DocumentID,
			VersionID:   chunk.VersionID,
			ArticleID:   chunk.ArticleID,
			CategoryID:  chunk.CategoryID,
			Version:     chunk.Version,
			EffectiveAt: effective,
			ExpiresAt:   expires,
			Score:       chunk.Score,
		},
	}, nil
}
