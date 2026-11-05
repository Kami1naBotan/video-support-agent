package generationquery

import (
	"context"
	"sync"
	"time"

	"video-support-agent/internal/retrieval"
)

type Retrievers struct {
	Keyword retrieval.Retriever
	Vector  retrieval.HydratedRetriever
	Hybrid  retrieval.HybridRetriever
}

type retrieverDependencies struct {
	resolver      Resolver
	es            ESSearcher
	vector        VectorSearcher
	verifier      retrieval.CurrentVerifier
	hydrator      retrieval.CurrentHydrator
	reranker      retrieval.Reranker
	vectorTimeout time.Duration
}

func NewRetrievers(resolver Resolver, es ESSearcher, vector VectorSearcher, verifier retrieval.CurrentVerifier, hydrator retrieval.CurrentHydrator, reranker retrieval.Reranker, vectorTimeout time.Duration) (*Retrievers, error) {
	if resolver == nil || es == nil || vector == nil ||
		verifier == nil || hydrator == nil ||
		vectorTimeout <= 0 {
		return nil, ErrTarget
	}

	deps := &retrieverDependencies{
		resolver:      resolver,
		es:            es,
		vector:        vector,
		verifier:      verifier,
		hydrator:      hydrator,
		reranker:      reranker,
		vectorTimeout: vectorTimeout,
	}

	return &Retrievers{
		Keyword: &keywordRetriever{deps: deps},
		Vector:  &vectorRetriever{deps: deps},
		Hybrid:  &hybridRetriever{deps: deps},
	}, nil
}

type requestBinding struct {
	deps *retrieverDependencies
	once sync.Once
	pair *Pair
	err  error
}

type requestCandidates struct {
	binding *requestBinding
	vector  bool
}

var _ retrieval.CandidateSearcher = (*requestCandidates)(nil)

func newRequestCandidates(deps *retrieverDependencies) (retrieval.CandidateSearcher, retrieval.CandidateSearcher) {
	binding := &requestBinding{deps: deps}

	es := &requestCandidates{
		binding: binding,
	}
	vector := &requestCandidates{
		binding: binding,
		vector:  true,
	}

	return es, vector
}

func (s *requestCandidates) SearchCandidates(ctx context.Context, query string, opts retrieval.SearchOptions, at time.Time) ([]retrieval.Candidate, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s == nil || s.binding == nil || s.binding.deps == nil {
		return nil, ErrTarget
	}

	binding := s.binding
	binding.once.Do(func() {
		deps := binding.deps
		binding.pair, binding.err = Bind(ctx, deps.resolver, deps.es, deps.vector, at)
	})

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if binding.err != nil {
		return nil, binding.err
	}
	if binding.pair == nil {
		return nil, ErrTarget
	}

	searcher := binding.pair.ES
	if s.vector {
		searcher = binding.pair.Vector
	}

	return searcher.SearchCandidates(ctx, query, opts, at)
}

type keywordRetriever struct {
	deps *retrieverDependencies
}

type vectorRetriever struct {
	deps *retrieverDependencies
}

var _ retrieval.Retriever = (*keywordRetriever)(nil)
var _ retrieval.HydratedRetriever = (*vectorRetriever)(nil)

func (r *keywordRetriever) Retrieve(ctx context.Context, query string, opts retrieval.SearchOptions) ([]retrieval.RuleChunk, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r == nil || r.deps == nil {
		return nil, ErrTarget
	}

	es, _ := newRequestCandidates(r.deps)

	service, err := retrieval.NewService(es, r.deps.verifier)
	if err != nil {
		return nil, err
	}

	return service.Retrieve(ctx, query, opts)
}

func (r *vectorRetriever) Retrieve(ctx context.Context, query string, opts retrieval.SearchOptions) ([]retrieval.HydratedChunk, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r == nil || r.deps == nil {
		return nil, ErrTarget
	}

	_, vector := newRequestCandidates(r.deps)

	service, err := retrieval.NewMilvusRetriever(vector, r.deps.hydrator, r.deps.vectorTimeout)
	if err != nil {
		return nil, err
	}

	return service.Retrieve(ctx, query, opts)
}

type hybridRetriever struct {
	deps *retrieverDependencies
}

var _ retrieval.HybridRetriever = (*hybridRetriever)(nil)

func (r *hybridRetriever) Retrieve(ctx context.Context, query string, opts retrieval.HybridOptions) (retrieval.HybridResult, error) {
	if err := ctx.Err(); err != nil {
		return retrieval.HybridResult{
			Outcome: retrieval.HybridFailed,
		}, err
	}
	if r == nil || r.deps == nil {
		return retrieval.HybridResult{
			Outcome:   retrieval.HybridFailed,
			ErrorKind: "configuration",
		}, retrieval.ErrHybridConfiguration
	}

	es, vector := newRequestCandidates(r.deps)

	service, err := retrieval.NewHybridService(es, vector, r.deps.hydrator, r.deps.reranker)
	if err != nil {
		return retrieval.HybridResult{
			Outcome:   retrieval.HybridFailed,
			ErrorKind: "configuration",
		}, err
	}

	return service.Retrieve(ctx, query, opts)
}
