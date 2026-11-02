package retrieval

import (
	"context"
	"fmt"
	"sort"
	"time"
)

const RRFSmoothing = 60

type FusionResult struct {
	Candidates []FusedCandidate

	RawCount       int
	DuplicateCount int
	ConflictCount  int
	UniqueCount    int
	ClippedCount   int
}

type fusionEntry struct {
	item       FusedCandidate
	seenSource [2]bool
	conflicted bool
}

func FuseCandidates(ctx context.Context, lists []RankedCandidates, opts HybridOptions, at time.Time) (FusionResult, error) {
	if err := ctx.Err(); err != nil {
		return FusionResult{}, err
	}

	opts, at, err := normalizeFusionOptions(opts, at)
	if err != nil {
		return FusionResult{}, err
	}

	if len(lists) < 1 || len(lists) > 2 {
		return FusionResult{}, fmt.Errorf("%w: fusion requires one or two successful sources", ErrInvalidCandidates)
	}

	bySource := make(
		map[RetrievalSource][]Candidate,
		len(lists),
	)

	rawCount := 0
	for _, list := range lists {
		if err := ctx.Err(); err != nil {
			return FusionResult{}, err
		}

		if _, exists := bySource[list.Source]; exists {
			return FusionResult{}, fmt.Errorf("%w: duplicate retrieval source", ErrInvalidCandidates)
		}

		frozen, err := FreezeRankedCandidates(ctx, list, opts, at)
		if err != nil {
			return FusionResult{}, err
		}

		bySource[frozen.Source] = frozen.Candidates
		rawCount += len(frozen.Candidates)
	}

	byID := make(map[uint64]*fusionEntry, rawCount)
	duplicateCount := 0
	conflictCount := 0

	sourceOrder := [2]RetrievalSource{
		SourceES,
		SourceMilvus,
	}

	for sourceIndex, source := range sourceOrder {
		for index, candidate := range bySource[source] {
			if err := ctx.Err(); err != nil {
				return FusionResult{}, err
			}

			id := candidate.Chunk.ChunkID
			entry, exists := byID[id]

			if !exists {
				entry = &fusionEntry{
					item: FusedCandidate{
						Candidate: candidate,
						Matches:   make([]SourceMatch, 0, 2),
					},
				}
				byID[id] = entry
			} else {
				duplicateCount++

				if !sameFusionIdentity(entry.item.Candidate, candidate) && !entry.conflicted {
					entry.conflicted = true
					conflictCount++
				}
			}

			if entry.conflicted || entry.seenSource[sourceIndex] {
				continue
			}

			entry.seenSource[sourceIndex] = true
			rank := index + 1

			entry.item.RRFScore += 1.0 /
				float64(RRFSmoothing+rank)

			entry.item.Matches = append(
				entry.item.Matches,
				SourceMatch{
					Source:   source,
					ChunkID:  id,
					Rank:     rank,
					RawScore: candidate.Chunk.Score,
				},
			)
		}
	}

	items := make([]FusedCandidate, 0, len(byID))
	for _, entry := range byID {
		if err := ctx.Err(); err != nil {
			return FusionResult{}, err
		}
		if entry.conflicted {
			continue
		}
		items = append(items, entry.item)
	}

	sort.Slice(items, func(i, j int) bool {
		if items[i].RRFScore != items[j].RRFScore {
			return items[i].RRFScore > items[j].RRFScore
		}
		return items[i].Candidate.Chunk.ChunkID <
			items[j].Candidate.Chunk.ChunkID
	})

	uniqueCount := len(items)
	clippedCount := 0

	if len(items) > MaxCandidates {
		clippedCount = len(items) - MaxCandidates
		items = items[:MaxCandidates]
	}

	if err := ctx.Err(); err != nil {
		return FusionResult{}, err
	}

	return FusionResult{
		Candidates:     items,
		RawCount:       rawCount,
		DuplicateCount: duplicateCount,
		ConflictCount:  conflictCount,
		UniqueCount:    uniqueCount,
		ClippedCount:   clippedCount,
	}, nil
}

func sameFusionIdentity(a, b Candidate) bool {
	left, right := a.Chunk, b.Chunk

	if left.ChunkID != right.ChunkID ||
		left.DocumentID != right.DocumentID ||
		left.VersionID != right.VersionID ||
		left.ArticleID != right.ArticleID ||
		left.CategoryID != right.CategoryID ||
		left.Version != right.Version ||
		a.PublicationRevision != b.PublicationRevision ||
		!left.EffectiveAt.Equal(right.EffectiveAt) {
		return false
	}

	if left.ExpiresAt == nil || right.ExpiresAt == nil {
		return left.ExpiresAt == nil && right.ExpiresAt == nil
	}

	return left.ExpiresAt.Equal(*right.ExpiresAt)
}
