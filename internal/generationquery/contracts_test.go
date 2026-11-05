package generationquery

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"video-support-agent/internal/embedding"
	"video-support-agent/internal/retrieval"
)

type contractResolver func(context.Context) (Target, error)

func (f contractResolver) ResolveActive(c context.Context) (Target, error) { return f(c) }

type contractES func(string, time.Time)

func (f contractES) SearchCandidatesAt(_ context.Context, name, q string, o retrieval.SearchOptions, at time.Time) ([]retrieval.Candidate, error) {
	f(name, at)
	return nil, nil
}

type contractVector func(string, embedding.ModelIdentity, time.Time)

func (f contractVector) SearchCandidatesAt(_ context.Context, name string, id embedding.ModelIdentity, q string, o retrieval.SearchOptions, at time.Time) ([]retrieval.Candidate, error) {
	f(name, id, at)
	return nil, nil
}
func contractTarget(n string) Target {
	id := "00000000-0000-4000-8000-00000000000" + n
	suffix := strings.ReplaceAll(id, "-", "")
	return Target{GenerationID: id, Epoch: 1, ESIndex: "knowledge_g_" + suffix, MilvusCollection: "rule_chunks_vec_v1_" + suffix, ManifestSHA256: strings.Repeat("a", 64), Identity: embedding.ModelIdentity{Model: embedding.ModelName, Digest: strings.Repeat("b", 64), Dimensions: embedding.Dimensions, ContextLength: embedding.ContextLength, InputPolicy: embedding.InputPolicy, ServerVersion: "test"}}
}

// Concurrent branches must bind exactly once; a new request must see the new active pair.
func TestRequestConcurrentPairAndRefresh(t *testing.T) {
	var calls atomic.Int32
	a, b := contractTarget("1"), contractTarget("2")
	var active atomic.Pointer[Target]
	active.Store(&a)
	var mu sync.Mutex
	var names []string
	at := time.Now().UTC().Truncate(time.Millisecond)
	record := func(name string, got time.Time) {
		mu.Lock()
		defer mu.Unlock()
		names = append(names, name)
		if !got.Equal(at) {
			t.Error("time changed")
		}
	}
	deps := &retrieverDependencies{resolver: contractResolver(func(context.Context) (Target, error) { calls.Add(1); return *active.Load(), nil }), es: contractES(record), vector: contractVector(func(n string, id embedding.ModelIdentity, got time.Time) {
		if id != a.Identity {
			t.Error("identity changed")
		}
		record(n, got)
	})}
	es, vec := newRequestCandidates(deps)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s := es
			if i%2 == 1 {
				s = vec
			}
			if _, err := s.SearchCandidates(context.Background(), "规则", retrieval.SearchOptions{}, at); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	active.Store(&b)
	if _, err := vec.SearchCandidates(context.Background(), "规则", retrieval.SearchOptions{}, at); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("resolve calls=%d", calls.Load())
	}
	for _, n := range names {
		if n != a.ESIndex && n != a.MilvusCollection {
			t.Fatalf("mixed target %s", n)
		}
	}
	es, _ = newRequestCandidates(deps)
	if _, err := es.SearchCandidates(context.Background(), "规则", retrieval.SearchOptions{}, at); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || names[len(names)-1] != b.ESIndex {
		t.Fatal("new request retained old target")
	}
}
func TestBindingEmptyErrorsAndCancellation(t *testing.T) {
	for _, mode := range []string{"empty", "unready", "corrupt", "cancelled", "time_mismatch"} {
		t.Run(mode, func(t *testing.T) {
			target := contractTarget("1")
			ctx := context.Background()
			at := time.Now()
			var called atomic.Int32
			resolver := contractResolver(func(context.Context) (Target, error) {
				called.Add(1)
				if mode == "unready" {
					return Target{}, retrieval.ErrIndexNotReady
				}
				return target, nil
			})
			if mode == "empty" {
				target.Empty = true
				target.ESIndex = ""
				target.MilvusCollection = ""
			}
			if mode == "corrupt" {
				target.ManifestSHA256 = "invalid"
			}
			if mode == "cancelled" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			pair, err := Bind(ctx, resolver, contractES(func(string, time.Time) { t.Error("backend unexpectedly called") }), contractVector(func(string, embedding.ModelIdentity, time.Time) { t.Error("backend unexpectedly called") }), at)
			switch mode {
			case "unready":
				if !errors.Is(err, retrieval.ErrIndexNotReady) {
					t.Fatal(err)
				}
				return
			case "corrupt":
				if !errors.Is(err, ErrTarget) {
					t.Fatal(err)
				}
				return
			case "cancelled":
				if !errors.Is(err, context.Canceled) || called.Load() != 0 {
					t.Fatalf("err=%v calls=%d", err, called.Load())
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if mode == "time_mismatch" {
				at = at.Add(time.Second)
			}
			for _, s := range []retrieval.CandidateSearcher{pair.ES, pair.Vector} {
				items, err := s.SearchCandidates(ctx, "规则", retrieval.SearchOptions{}, at)
				if mode == "time_mismatch" {
					if !errors.Is(err, ErrTarget) {
						t.Fatal(err)
					}
				} else if err != nil || len(items) != 0 {
					t.Fatalf("items=%v err=%v", items, err)
				}
			}
		})
	}
}

type contractCurrent struct{}

func (contractCurrent) VerifyCurrent(context.Context, []retrieval.Candidate, time.Time) (map[uint64]struct{}, error) {
	return map[uint64]struct{}{}, nil
}
func (contractCurrent) HydrateCurrent(context.Context, []retrieval.Candidate, time.Time) (map[uint64]retrieval.HydratedChunk, error) {
	return map[uint64]retrieval.HydratedChunk{}, nil
}

// Exercise the public wrappers, so an accidentally process-scoped binding is detected.
func TestPublicRetrieversRefreshAndEmpty(t *testing.T) {
	for _, kind := range []string{"keyword", "vector", "hybrid"} {
		t.Run(kind, func(t *testing.T) {
			var calls atomic.Int32
			a, b := contractTarget("1"), contractTarget("2")
			var active atomic.Pointer[Target]
			active.Store(&a)
			var mu sync.Mutex
			var names []string
			record := func(n string, _ time.Time) { mu.Lock(); defer mu.Unlock(); names = append(names, n) }
			r, err := NewRetrievers(contractResolver(func(context.Context) (Target, error) { calls.Add(1); return *active.Load(), nil }), publicContractES{record}, publicContractVector{record}, contractCurrent{}, contractCurrent{}, nil, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			run := func() {
				t.Helper()
				var err error
				switch kind {
				case "keyword":
					_, err = r.Keyword.Retrieve(context.Background(), "规则", retrieval.SearchOptions{})
				case "vector":
					_, err = r.Vector.Retrieve(context.Background(), "规则", retrieval.SearchOptions{})
				case "hybrid":
					_, err = r.Hybrid.Retrieve(context.Background(), "规则", retrieval.HybridOptions{})
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			run()
			if calls.Load() != 1 {
				t.Fatal("first request resolved more than once")
			}
			names = nil
			active.Store(&b)
			run()
			if calls.Load() != 2 {
				t.Fatal("second request did not resolve once")
			}
			for _, n := range names {
				if n != b.ESIndex && n != b.MilvusCollection {
					t.Fatal("stale target")
				}
			}
			b.Empty = true
			b.ESIndex = ""
			b.MilvusCollection = ""
			names = nil
			run()
			if calls.Load() != 3 || len(names) != 0 {
				t.Fatal("empty target contacted backend or skipped resolve")
			}
		})
	}
}

type publicContractES struct{ record func(string, time.Time) }

func (s publicContractES) SearchCandidatesAt(_ context.Context, n, q string, o retrieval.SearchOptions, at time.Time) ([]retrieval.Candidate, error) {
	s.record(n, at)
	return []retrieval.Candidate{}, nil
}

type publicContractVector struct{ record func(string, time.Time) }

func (s publicContractVector) SearchCandidatesAt(_ context.Context, n string, id embedding.ModelIdentity, q string, o retrieval.SearchOptions, at time.Time) ([]retrieval.Candidate, error) {
	s.record(n, at)
	return []retrieval.Candidate{}, nil
}
