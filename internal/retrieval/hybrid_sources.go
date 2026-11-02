package retrieval

import (
	"context"
	"errors"
	"time"
)

type ParallelCandidates struct {
	es     CandidateSearcher
	milvus CandidateSearcher
}

type candidateSourceReply struct {
	index  int
	list   RankedCandidates
	report SourceReport
}

func NewParallelCandidates(es CandidateSearcher, milvus CandidateSearcher) (*ParallelCandidates, error) {
	if es == nil || milvus == nil {
		return nil, errors.New("ES and Milvus candidate searchers are required")
	}

	return &ParallelCandidates{
		es:     es,
		milvus: milvus,
	}, nil
}

func (p *ParallelCandidates) Collect(ctx context.Context, query string, opts HybridOptions, at time.Time) (ParallelCandidateResult, error) {
	if err := ctx.Err(); err != nil {
		return ParallelCandidateResult{}, err
	}
	if p == nil || p.es == nil || p.milvus == nil {
		return ParallelCandidateResult{}, errors.New("parallel candidate collector is not configured")
	}

	query, opts, err := NormalizeHybridSearch(query, opts)
	if err != nil {
		return ParallelCandidateResult{}, err
	}

	opts, at, err = normalizeFusionOptions(opts, at)
	if err != nil {
		return ParallelCandidateResult{}, err
	}

	started := time.Now()
	phaseCtx, cancel := context.WithTimeout(ctx, HybridSourceTimeout)
	defer cancel()

	if err := ctx.Err(); err != nil {
		return ParallelCandidateResult{}, err
	}

	sources := [2]RetrievalSource{
		SourceES,
		SourceMilvus,
	}
	searchers := [2]CandidateSearcher{
		p.es,
		p.milvus,
	}

	result := ParallelCandidateResult{}
	for index, source := range sources {
		result.Sources[index] = SourceReport{
			Source: source,
			Status: SourceRunning,
		}
	}

	replies := make(chan candidateSourceReply, 2)

	for index, searcher := range searchers {
		go runCandidateSource(phaseCtx, replies, index, sources[index], searcher, query, opts, at, started)
	}

	var received [2]bool
	var lists [2]RankedCandidates
	receivedCount := 0

	accept := func(reply candidateSourceReply) {
		if received[reply.index] {
			return
		}

		received[reply.index] = true
		receivedCount++
		result.Sources[reply.index] = reply.report
		lists[reply.index] = reply.list
	}

	for receivedCount < len(sources) {
		select {
		case reply := <-replies:
			accept(reply)

		case <-phaseCtx.Done():
		drain:
			for receivedCount < len(sources) {
				select {
				case reply := <-replies:
					accept(reply)
				default:
					break drain
				}
			}

			for index, source := range sources {
				if received[index] {
					continue
				}

				result.Sources[index] = SourceReport{
					Source:        source,
					Status:        SourceFailed,
					Duration:      time.Since(started),
					RawCountKnown: false,
					ErrorKind: classifySourceError(
						phaseCtx.Err(),
					),
				}
			}

			receivedCount = len(sources)
		}
	}

	result.Duration = time.Since(started)

	if err := ctx.Err(); err != nil {
		return result, err
	}

	successful := make([]RankedCandidates, 0, len(sources))
	for index, report := range result.Sources {
		if report.Status == SourceSucceeded || report.Status == SourceEmpty {
			successful = append(successful, lists[index])
		}
	}

	if len(successful) == 0 {
		return result, ErrHybridUnavailable
	}

	result.Lists = successful
	result.Degraded = len(successful) == 1

	if err := ctx.Err(); err != nil {
		result.Lists = nil
		return result, err
	}
	return result, nil
}

func runCandidateSource(ctx context.Context, replies chan<- candidateSourceReply, index int, source RetrievalSource, searcher CandidateSearcher, query string, opts HybridOptions, at time.Time, started time.Time) {
	candidates, err := searcher.SearchCandidates(ctx, query, opts.searchOptions(), at)

	if contextErr := ctx.Err(); contextErr != nil {
		err = contextErr
	}

	var frozen RankedCandidates
	if err == nil {
		frozen, err = FreezeRankedCandidates(
			ctx,
			RankedCandidates{
				Source:     source,
				Candidates: candidates,
			},
			opts,
			at,
		)
	}

	finished := time.Now()

	if contextErr := ctx.Err(); contextErr != nil {
		err = contextErr
	} else if deadline, ok := ctx.Deadline(); ok &&
		!finished.Before(deadline) {
		err = context.DeadlineExceeded
	}

	report := SourceReport{
		Source:        source,
		Status:        SourceFailed,
		Duration:      finished.Sub(started),
		RawCount:      len(candidates),
		RawCountKnown: true,
		ErrorKind:     classifySourceError(err),
	}

	if err == nil {
		report.Status = SourceSucceeded
		if len(frozen.Candidates) == 0 {
			report.Status = SourceEmpty
		}
	} else {
		frozen = RankedCandidates{}
	}

	replies <- candidateSourceReply{
		index:  index,
		list:   frozen,
		report: report,
	}
}

func classifySourceError(err error) SourceErrorKind {
	switch {
	case err == nil:
		return SourceErrorNone
	case errors.Is(err, context.Canceled):
		return SourceErrorCanceled
	case errors.Is(err, context.DeadlineExceeded):
		return SourceErrorTimeout
	case errors.Is(err, ErrInvalidCandidates):
		return SourceErrorInvalidCandidates
	case errors.Is(err, ErrIndexNotReady):
		return SourceErrorIndexNotReady
	default:
		return SourceErrorDependency
	}
}
