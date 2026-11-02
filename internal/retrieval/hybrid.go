package retrieval

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type HybridService struct {
	sources  *ParallelCandidates
	hydrator CurrentHydrator
	reranker Reranker
	clock    func() time.Time
}

var _ HybridRetriever = (*HybridService)(nil)

func NewHybridService(es CandidateSearcher, milvus CandidateSearcher, hydrator CurrentHydrator, reranker Reranker) (*HybridService, error) {
	return NewHybridServiceWithClock(es, milvus, hydrator, reranker, time.Now)
}

func NewHybridServiceWithClock(es CandidateSearcher, milvus CandidateSearcher, hydrator CurrentHydrator, reranker Reranker, clock func() time.Time) (*HybridService, error) {
	if hydrator == nil || clock == nil {
		return nil, ErrHybridConfiguration
	}

	sources, err := NewParallelCandidates(es, milvus)
	if err != nil {
		return nil, ErrHybridConfiguration
	}

	return &HybridService{
		sources:  sources,
		hydrator: hydrator,
		reranker: reranker,
		clock:    clock,
	}, nil
}

func (s *HybridService) Retrieve(ctx context.Context, query string, opts HybridOptions) (result HybridResult, err error) {
	started := time.Now()

	result.Outcome = HybridFailed
	result.Stats.Sources = [2]SourceReport{
		{Source: SourceES, Status: SourceNotStarted},
		{Source: SourceMilvus, Status: SourceNotStarted},
	}
	result.Stats.Fusion.Status = HybridStageNotRun
	result.Stats.Hydration.Status = HybridStageNotRun
	result.Stats.Rerank.Status = HybridStageNotRun

	opCtx, cancel := context.WithTimeout(ctx, HybridTimeout)
	defer cancel()

	defer func() {
		if contextErr := hybridContextError(ctx); contextErr != nil {
			err = contextErr
		} else if contextErr := hybridContextError(opCtx); contextErr != nil {
			err = contextErr
		}

		result.Duration = time.Since(started)
		result.ErrorKind = classifyHybridError(err)

		if err != nil {
			result.Items = nil
			result.Outcome = HybridFailed
			result.EmptyReason = ""
			result.Stats.FinalCount = 0
		}
	}()

	if err = hybridContextError(opCtx); err != nil {
		return result, err
	}
	if s == nil || s.sources == nil || s.hydrator == nil || s.clock == nil {
		return result, ErrHybridConfiguration
	}

	query, opts, err = NormalizeHybridSearch(query, opts)
	if err != nil {
		return result, err
	}

	at := s.clock().UTC().Truncate(time.Millisecond)
	if at.IsZero() || at.Year() < 1 || at.Year() > 9999 {
		return result, ErrHybridConfiguration
	}
	result.RetrievedAt = at

	parallel, err := s.sources.Collect(opCtx, query, opts, at)
	for index, report := range parallel.Sources {
		if report.Source != "" {
			result.Stats.Sources[index] = report
		}
	}
	result.Stats.SourceDuration = parallel.Duration
	result.Degraded = parallel.Degraded
	if err != nil {
		return result, err
	}

	fusionStarted := time.Now()
	fused, err := FuseCandidates(opCtx, parallel.Lists, opts, at)
	result.Stats.Fusion = makeHybridStageReport(fusionStarted, err)
	if err != nil {
		return result, err
	}

	result.Stats.RawCount = fused.RawCount
	result.Stats.DuplicateCount = fused.DuplicateCount
	result.Stats.ConflictCount = fused.ConflictCount
	result.Stats.UniqueCount = fused.UniqueCount
	result.Stats.ClippedCount = fused.ClippedCount
	result.Stats.FusedCount = len(fused.Candidates)

	if len(fused.Candidates) == 0 {
		result.Stats.Hydration.Status = HybridStageSkipped
		result.Stats.Rerank.Status = HybridStageSkipped

		reason := "no_candidates"
		if fused.ConflictCount > 0 {
			reason = "conflicts"
		}
		completeHybridResult(&result, nil, opts.TopK, reason)
		return result, nil
	}

	items, hydratedCount, hydrationReport, err := s.hydrate(opCtx, fused.Candidates, opts, at)
	result.Stats.Hydration = hydrationReport
	if err != nil {
		return result, err
	}

	result.Stats.HydratedCount = hydratedCount
	result.Stats.FilteredCount = len(fused.Candidates) - hydratedCount
	result.Stats.ArticleCount = len(items)

	if len(items) == 0 {
		result.Stats.Rerank.Status = HybridStageSkipped
		completeHybridResult(&result, items, opts.TopK, "current_filter")
		return result, nil
	}

	items, result.Stats.Rerank, err = s.rerank(opCtx, query, items)
	if err != nil {
		return result, err
	}

	completeHybridResult(&result, items, opts.TopK, "")
	return result, nil
}

func (s *HybridService) hydrate(ctx context.Context, fused []FusedCandidate, opts HybridOptions, at time.Time) (items []HybridItem, count int, report HybridStageReport, err error) {
	started := time.Now()
	stageCtx, cancel := context.WithTimeout(ctx, HybridHydrationTimeout)
	defer cancel()

	defer func() {
		parentErr := hybridContextError(ctx)
		if parentErr != nil {
			err = parentErr
		} else if contextErr := hybridContextError(stageCtx); contextErr != nil {
			err = contextErr
		}

		report = makeHybridStageReport(started, err)

		if err != nil {
			items = nil
			count = 0

			if parentErr == nil {
				if errors.Is(err, ErrHydrationLimit) {
					err = fmt.Errorf("%w: %w", ErrHydrationFailed, ErrHydrationLimit)
				} else {
					err = ErrHydrationFailed
				}
			}
		}
	}()

	prepared, err := PrepareHybridHydration(stageCtx, fused, opts, at)
	if err != nil {
		return nil, 0, report, err
	}

	hydrated, err := callHybridStage(
		stageCtx,
		func(callCtx context.Context) (map[uint64]HydratedChunk, error) {
			return s.hydrator.HydrateCurrent(callCtx, prepared, at)
		},
	)
	if err != nil {
		return nil, 0, report, err
	}

	items, err = AssembleHybridItems(stageCtx, fused, hydrated, opts, at)
	if err != nil {
		return nil, 0, report, err
	}

	count = len(hydrated)
	return items, count, report, nil
}

func (s *HybridService) rerank(ctx context.Context, query string, items []HybridItem) (output []HybridItem, report HybridStageReport, err error) {
	if s.reranker == nil {
		return items, HybridStageReport{
			Status: HybridStageSkipped,
		}, nil
	}

	started := time.Now()
	stageCtx, cancel := context.WithTimeout(ctx, HybridRerankTimeout)
	defer cancel()

	allowFallback := true

	defer func() {
		parentErr := hybridContextError(ctx)
		if parentErr != nil {
			err = parentErr
		} else if contextErr := hybridContextError(stageCtx); contextErr != nil {
			err = contextErr
		}

		report = makeHybridStageReport(started, err)

		if err != nil {
			if parentErr == nil && allowFallback {
				output = items
				report.Status = HybridStageFallback
				err = nil
			} else {
				output = nil
			}
		}
	}()

	documents, err := BuildRerankDocuments(stageCtx, items)
	if err != nil {
		allowFallback = errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
		return nil, report, err
	}

	order, err := callHybridStage(
		stageCtx,
		func(callCtx context.Context) ([]uint64, error) {
			return s.reranker.Rerank(callCtx, query, documents)
		},
	)
	if err != nil {
		return nil, report, err
	}

	output, err = ApplyRerankOrder(stageCtx, items, order)
	return output, report, err
}

func completeHybridResult(result *HybridResult, items []HybridItem, topK int, emptyReason string) {
	count := len(items)
	if count > topK {
		count = topK
	}

	result.Items = make([]HybridItem, count)
	copy(result.Items, items[:count])
	result.Stats.FinalCount = count
	result.EmptyReason = ""

	if count > 0 {
		result.Outcome = HybridSuccess
		if result.Degraded {
			result.Outcome = HybridDegraded
		}
		return
	}

	result.EmptyReason = emptyReason
	switch {
	case emptyReason == "conflicts":
		result.Outcome = HybridNoUsableEvidence
	case result.Degraded:
		result.Outcome = HybridDegradedEmpty
	default:
		result.Outcome = HybridNoEvidence
	}
}
