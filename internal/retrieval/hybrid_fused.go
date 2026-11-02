package retrieval

import (
	"context"
	"fmt"
	"time"
)

func freezeFusedCandidates(ctx context.Context, fused []FusedCandidate, opts HybridOptions, at time.Time) ([]FusedCandidate, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	opts, at, err := normalizeFusionOptions(opts, at)
	if err != nil {
		return nil, err
	}

	limit := 2 * opts.TopK * CandidateMultiplier
	if limit > MaxCandidates {
		limit = MaxCandidates
	}
	if fused == nil || len(fused) > limit {
		return nil, fmt.Errorf("%w: invalid fused candidate count", ErrInvalidCandidates)
	}

	result := make([]FusedCandidate, 0, len(fused))
	seen := make(map[uint64]struct{}, len(fused))

	for _, item := range fused {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		if len(item.Matches) < 1 || len(item.Matches) > 2 {
			return nil, fmt.Errorf("%w: invalid fused source matches", ErrInvalidCandidates)
		}

		var candidate Candidate
		score := 0.0
		previousSource := -1

		for index, match := range item.Matches {
			sourceIndex := -1
			switch match.Source {
			case SourceES:
				sourceIndex = 0
			case SourceMilvus:
				sourceIndex = 1
			default:
				return nil, fmt.Errorf("%w: unknown fused source", ErrInvalidCandidates)
			}

			if sourceIndex <= previousSource {
				return nil, fmt.Errorf("%w: repeated or unordered fused sources", ErrInvalidCandidates)
			}
			previousSource = sourceIndex

			if match.ChunkID != item.Candidate.Chunk.ChunkID || match.Rank < 1 || match.Rank > opts.TopK*CandidateMultiplier {
				return nil, fmt.Errorf("%w: invalid fused match identity or rank", ErrInvalidCandidates)
			}

			probe := item.Candidate
			probe.Chunk.Score = match.RawScore

			frozen, err := freezeFusionCandidate(probe, match.Source, opts, at)
			if err != nil {
				return nil, err
			}

			if index == 0 {
				if item.Candidate.Chunk.Score != match.RawScore {
					return nil, fmt.Errorf("%w: fused candidate raw score differs", ErrInvalidCandidates)
				}
				candidate = frozen
			}

			score += 1.0 / float64(RRFSmoothing+match.Rank)
		}

		if item.RRFScore != score {
			return nil, fmt.Errorf("%w: fused RRF score differs", ErrInvalidCandidates)
		}

		id := candidate.Chunk.ChunkID
		if _, exists := seen[id]; exists {
			return nil, fmt.Errorf("%w: duplicate fused chunk", ErrInvalidCandidates)
		}
		seen[id] = struct{}{}

		if len(result) > 0 {
			previous := result[len(result)-1]
			if score > previous.RRFScore || (score == previous.RRFScore && id < previous.Candidate.Chunk.ChunkID) {
				return nil, fmt.Errorf("%w: fused candidates are out of order", ErrInvalidCandidates)
			}
		}

		matches := make([]SourceMatch, len(item.Matches))
		copy(matches, item.Matches)

		result = append(result, FusedCandidate{
			Candidate: candidate,
			RRFScore:  score,
			Matches:   matches,
		})
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}
