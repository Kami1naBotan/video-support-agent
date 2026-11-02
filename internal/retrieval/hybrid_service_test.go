package retrieval_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"video-support-agent/internal/retrieval"
)

type ragRerankFunc func(
	context.Context, string, []retrieval.RerankDocument,
) ([]uint64, error)

func (f ragRerankFunc) Rerank(
	ctx context.Context, query string, docs []retrieval.RerankDocument,
) ([]uint64, error) {
	return f(ctx, query, docs)
}

func ragHydrate(
	_ context.Context,
	candidates []retrieval.Candidate,
	_ time.Time,
) (map[uint64]retrieval.HydratedChunk, error) {
	rows := make(map[uint64]retrieval.HydratedChunk, len(candidates))
	for _, candidate := range candidates {
		rows[candidate.Chunk.ChunkID] = ragHydrationRow(candidate)
	}
	return rows, nil
}

func ragService(
	t *testing.T,
	es, mv retrieval.CandidateSearcher,
	hydrator retrieval.CurrentHydrator,
	reranker retrieval.Reranker,
) *retrieval.HybridService {
	t.Helper()
	service, err := retrieval.NewHybridServiceWithClock(
		es, mv, hydrator, reranker, ragFusionAt,
	)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func ragServiceFixture(
	t *testing.T,
	hydrator retrieval.CurrentHydrator,
	reranker retrieval.Reranker,
) *retrieval.HybridService {
	t.Helper()
	if hydrator == nil {
		hydrator = vrHydrator(ragHydrate)
	}
	return ragService(t,
		ragSource([]retrieval.Candidate{
			ragFusionCandidate(1, 0.9),
			ragFusionCandidate(2, 0.8),
		}, nil),
		ragSource([]retrieval.Candidate{}, nil),
		hydrator, reranker,
	)
}

func ragFailed(
	t *testing.T,
	result retrieval.HybridResult,
	err, want error,
) {
	t.Helper()
	if !errors.Is(err, want) ||
		result.Items != nil ||
		result.Outcome != retrieval.HybridFailed ||
		result.Stats.FinalCount != 0 ||
		result.EmptyReason != "" ||
		result.ErrorKind == "" {
		t.Fatalf("failed result=%+v error=%v want=%v", result, err, want)
	}
}

func TestRAGService_Flow(t *testing.T) {
	es := []retrieval.Candidate{
		ragFusionCandidate(1, 9),
		ragFusionCandidate(1, 8),
		ragFusionCandidate(2, 7),
		ragFusionCandidate(3, 6),
		ragFusionCandidate(4, 5),
	}
	es[2].Chunk.ArticleID = es[0].Chunk.ArticleID
	mv := []retrieval.Candidate{
		ragFusionCandidate(3, 0.9),
		ragFusionCandidate(1, 0.8),
		ragFusionCandidate(4, 0.7),
		ragFusionCandidate(5, 0.6),
	}
	mv[2].PublicationRevision++

	beforeES := ragFusionSnapshot(t, es)
	beforeMV := ragFusionSnapshot(t, mv)
	type requestKey struct{}
	ctx := context.WithValue(context.Background(), requestKey{}, "marker")
	wantOptions := retrieval.SearchOptions{
		DocumentID: 11, VersionID: 21, CategoryID: 41, Limit: 1,
	}
	var searchCalls, hydrateCalls, rerankCalls, clockCalls atomic.Int32
	prepared := make(chan []retrieval.Candidate, 1)

	source := func(items []retrieval.Candidate) retrieval.CandidateSearcher {
		return vrSearcher(func(
			callCtx context.Context, query string,
			opts retrieval.SearchOptions, at time.Time,
		) ([]retrieval.Candidate, error) {
			searchCalls.Add(1)
			if callCtx.Value(requestKey{}) != "marker" ||
				query != "测试" || opts != wantOptions ||
				at != ragFusionAt() || at.Location() != time.UTC {
				t.Error("source lost normalized input, filters, time or context")
			}
			return items, nil
		})
	}
	hydrator := vrHydrator(func(
		callCtx context.Context, candidates []retrieval.Candidate, at time.Time,
	) (map[uint64]retrieval.HydratedChunk, error) {
		hydrateCalls.Add(1)
		if callCtx.Value(requestKey{}) != "marker" || at != ragFusionAt() {
			t.Error("hydration lost shared time or context")
		}
		prepared <- candidates
		rows, _ := ragHydrate(callCtx, candidates, at)
		delete(rows, 3)
		return rows, nil
	})
	reranker := ragRerankFunc(func(
		callCtx context.Context, query string, docs []retrieval.RerankDocument,
	) ([]uint64, error) {
		rerankCalls.Add(1)
		if callCtx.Value(requestKey{}) != "marker" || query != "测试" {
			t.Error("rerank lost normalized query or context")
		}
		if len(docs) != 2 || docs[0].ChunkID != 1 || docs[1].ChunkID != 5 {
			t.Error("rerank did not receive all merged articles before TopK")
			return nil, retrieval.ErrInvalidRerank
		}
		for _, doc := range docs {
			if doc.ArticleContent != "isolated complete article" ||
				doc.Title != "回表测试规则" {
				t.Error("rerank used index text instead of hydrated article")
			}
		}
		return []uint64{5, 1}, nil
	})
	service, err := retrieval.NewHybridServiceWithClock(
		source(es), source(mv), hydrator, reranker,
		func() time.Time {
			clockCalls.Add(1)
			return ragFusionAt().Add(123456 * time.Nanosecond).
				In(time.FixedZone("fixture", 8*60*60))
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	got, err := service.Retrieve(ctx, "  测试  ", retrieval.HybridOptions{
		DocumentID: 11, VersionID: 21, CategoryID: 41, TopK: 1,
	})
	if err != nil || got.Outcome != retrieval.HybridSuccess ||
		got.Degraded || got.ErrorKind != "" ||
		got.RetrievedAt != ragFusionAt() ||
		len(got.Items) != 1 || got.Items[0].Rule.Chunk.ChunkID != 5 {
		t.Fatalf("flow result=%+v error=%v", got, err)
	}
	if searchCalls.Load() != 2 || hydrateCalls.Load() != 1 ||
		rerankCalls.Load() != 1 || clockCalls.Load() != 1 {
		t.Fatal("a dependency was skipped or repeated")
	}

	input := ragTake(t, prepared)
	if len(input) != 4 {
		t.Fatalf("hydration candidate count=%d", len(input))
	}
	wantIDs := []uint64{1, 3, 2, 5}
	wantScores := []float64{
		1.0/61 + 1.0/62,
		1.0/64 + 1.0/61,
		1.0 / 63,
		1.0 / 64,
	}
	for i, candidate := range input {
		if candidate.Chunk.ChunkID != wantIDs[i] ||
			candidate.Chunk.Content != "" || candidate.Chunk.Title != "" {
			t.Fatal("hydration input identity/order/text differs")
		}
		ragFusionScore(t, candidate.Chunk.Score, wantScores[i])
	}
	ragFusionScore(t, got.Items[0].RRFScore, 1.0/64)
	if got.Items[0].Rule.ArticleContent != "isolated complete article" {
		t.Fatal("final result lost hydrated article")
	}

	stats := got.Stats
	actual := []int{
		stats.RawCount, stats.DuplicateCount, stats.ConflictCount,
		stats.UniqueCount, stats.ClippedCount, stats.FusedCount,
		stats.HydratedCount, stats.FilteredCount,
		stats.ArticleCount, stats.FinalCount,
	}
	if !reflect.DeepEqual(actual, []int{9, 4, 1, 4, 0, 4, 3, 1, 2, 1}) {
		t.Fatalf("counts=%v", actual)
	}
	for _, stage := range []retrieval.HybridStageReport{
		stats.Fusion, stats.Hydration, stats.Rerank,
	} {
		if stage.Status != retrieval.HybridStageSuccess ||
			stage.ErrorKind != "" || stage.Duration < 0 {
			t.Fatalf("stage=%+v", stage)
		}
	}
	if stats.Sources[0].RawCount != 5 || stats.Sources[1].RawCount != 4 {
		t.Fatal("source counts changed")
	}
	if beforeES != ragFusionSnapshot(t, es) ||
		beforeMV != ragFusionSnapshot(t, mv) {
		t.Fatal("service modified source storage")
	}
}

func TestRAGService_Outcomes(t *testing.T) {
	for _, mode := range []string{
		"empty", "degraded_empty", "degraded_success",
		"both_failed", "conflicts", "filtered", "degraded_filtered",
	} {
		t.Run(mode, func(t *testing.T) {
			es := []retrieval.Candidate{}
			mv := []retrieval.Candidate{}
			var esErr, mvErr error
			wantOutcome := retrieval.HybridNoEvidence
			wantReason := "no_candidates"
			wantHydration := 0
			wantRerank := 0
			degraded := false

			switch mode {
			case "degraded_empty":
				esErr = errors.New("synthetic ES failure")
				wantOutcome = retrieval.HybridDegradedEmpty
				degraded = true
			case "degraded_success", "degraded_filtered":
				esErr = errors.New("synthetic ES failure")
				mv = []retrieval.Candidate{ragFusionCandidate(1, 0.8)}
				degraded = true
				wantHydration = 1
				wantReason = ""
				wantOutcome = retrieval.HybridDegraded
				wantRerank = 1
				if mode == "degraded_filtered" {
					wantOutcome = retrieval.HybridDegradedEmpty
					wantReason = "current_filter"
					wantRerank = 0
				}
			case "both_failed":
				esErr = errors.New("synthetic ES failure")
				mvErr = errors.New("synthetic Milvus failure")
				wantOutcome = retrieval.HybridFailed
				wantReason = ""
			case "conflicts":
				es = []retrieval.Candidate{ragFusionCandidate(1, 0.8)}
				mv = []retrieval.Candidate{ragFusionCandidate(1, 0.8)}
				mv[0].PublicationRevision++
				wantOutcome = retrieval.HybridNoUsableEvidence
				wantReason = "conflicts"
			case "filtered":
				es = []retrieval.Candidate{ragFusionCandidate(1, 0.8)}
				wantHydration = 1
				wantReason = "current_filter"
			}

			var hydCalls, rrCalls atomic.Int32
			hydrator := vrHydrator(func(
				ctx context.Context, items []retrieval.Candidate, at time.Time,
			) (map[uint64]retrieval.HydratedChunk, error) {
				hydCalls.Add(1)
				if mode == "filtered" || mode == "degraded_filtered" {
					return map[uint64]retrieval.HydratedChunk{}, nil
				}
				return ragHydrate(ctx, items, at)
			})
			reranker := ragRerankFunc(func(
				context.Context, string, []retrieval.RerankDocument,
			) ([]uint64, error) {
				rrCalls.Add(1)
				return []uint64{1}, nil
			})
			service := ragService(t,
				ragSource(es, esErr), ragSource(mv, mvErr),
				hydrator, reranker,
			)
			got, err := service.Retrieve(
				context.Background(), "测试", retrieval.HybridOptions{},
			)
			if mode == "both_failed" {
				ragFailed(t, got, err, retrieval.ErrHybridUnavailable)
				if got.Stats.Fusion.Status != retrieval.HybridStageNotRun ||
					got.Stats.Hydration.Status != retrieval.HybridStageNotRun {
					t.Fatal("unavailable sources still advanced the pipeline")
				}
			} else {
				if err != nil || got.Items == nil ||
					got.Outcome != wantOutcome ||
					got.EmptyReason != wantReason ||
					got.Degraded != degraded {
					t.Fatalf("result=%+v error=%v", got, err)
				}
				if wantHydration == 0 &&
					got.Stats.Hydration.Status != retrieval.HybridStageSkipped {
					t.Fatal("empty fusion did not skip hydration")
				}
				if wantRerank == 0 &&
					got.Stats.Rerank.Status != retrieval.HybridStageSkipped {
					t.Fatal("empty evidence did not skip rerank")
				}
				if got.Stats.FinalCount != wantRerank ||
					len(got.Items) != wantRerank {
					t.Fatal("incorrect final item count")
				}
			}
			if int(hydCalls.Load()) != wantHydration ||
				int(rrCalls.Load()) != wantRerank {
				t.Fatal("unexpected downstream dependency invocation")
			}
		})
	}
}

func TestRAGService_HydrationFailure(t *testing.T) {
	for _, mode := range []string{
		"dependency", "nil_map", "mismatch", "extra", "limit", "timeout",
	} {
		t.Run(mode, func(t *testing.T) {
			release := make(chan struct{})
			exited := make(chan struct{}, 1)
			dependencyErr := errors.New("synthetic private hydration error")
			before := time.Now()
			var rrCalls atomic.Int32

			hydrator := vrHydrator(func(
				ctx context.Context, items []retrieval.Candidate, at time.Time,
			) (map[uint64]retrieval.HydratedChunk, error) {
				defer func() { exited <- struct{}{} }()
				ragCheckDeadline(t, ctx, before, 3*time.Second)
				rows, _ := ragHydrate(ctx, items, at)
				switch mode {
				case "dependency":
					return rows, dependencyErr
				case "nil_map":
					return nil, nil
				case "mismatch":
					row := rows[1]
					row.Chunk.VersionID++
					rows[1] = row
				case "extra":
					rows[99] = ragHydrationRow(ragFusionCandidate(99, 0.1))
				case "limit":
					row := rows[1]
					row.ArticleContent = strings.Repeat(
						"x", retrieval.MaxHydrationRowBytes+1,
					)
					rows[1] = row
				case "timeout":
					// 故意不响应 ctx，验证编排层能停止等待。
					<-release
				}
				return rows, nil
			})
			reranker := ragRerankFunc(func(
				context.Context, string, []retrieval.RerankDocument,
			) ([]uint64, error) {
				rrCalls.Add(1)
				return []uint64{1, 2}, nil
			})
			got, err := ragServiceFixture(t, hydrator, reranker).Retrieve(
				context.Background(), "测试", retrieval.HybridOptions{},
			)
			close(release)
			ragTake(t, exited)

			ragFailed(t, got, err, retrieval.ErrHydrationFailed)
			wantKind := "hydration"
			switch mode {
			case "dependency":
				wantKind = "dependency"
			case "limit":
				wantKind = "hydration_limit"
				if !errors.Is(err, retrieval.ErrHydrationLimit) {
					t.Fatal("budget sentinel was lost")
				}
			case "timeout":
				wantKind = "timeout"
			}
			if got.Stats.Hydration.Status != retrieval.HybridStageFailed ||
				got.Stats.Hydration.ErrorKind != wantKind ||
				got.Stats.Rerank.Status != retrieval.HybridStageNotRun ||
				rrCalls.Load() != 0 {
				t.Fatalf("hydration failure stats=%+v", got.Stats)
			}
			if errors.Is(err, dependencyErr) ||
				errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("child hydration error escaped as the overall error")
			}
		})
	}
}

func TestRAGService_RerankFallback(t *testing.T) {
	for _, mode := range []string{
		"dependency", "nil", "missing", "duplicate", "unknown", "timeout",
	} {
		t.Run(mode, func(t *testing.T) {
			release := make(chan struct{})
			exited := make(chan struct{}, 1)
			before := time.Now()
			reranker := ragRerankFunc(func(
				ctx context.Context, _ string, docs []retrieval.RerankDocument,
			) ([]uint64, error) {
				defer func() { exited <- struct{}{} }()
				ragCheckDeadline(t, ctx, before, time.Second)
				if mode == "timeout" {
					<-release
				}
				// 插件输入必须独立于回退结果。
				if len(docs) != 2 {
					t.Error("unexpected rerank input count")
					return nil, retrieval.ErrInvalidRerank
				}
				docs[0].Title = "plugin-only"
				docs[0].ArticleContent = "plugin-only"
				switch mode {
				case "dependency":
					return nil, errors.New("synthetic rerank failure")
				case "nil":
					return nil, nil
				case "missing":
					return []uint64{1}, nil
				case "duplicate":
					return []uint64{1, 1}, nil
				case "unknown":
					return []uint64{1, 99}, nil
				default:
					return []uint64{2, 1}, nil
				}
			})

			got, err := ragServiceFixture(t, nil, reranker).Retrieve(
				context.Background(), "测试", retrieval.HybridOptions{},
			)
			close(release)
			ragTake(t, exited)

			if err != nil || got.Outcome != retrieval.HybridSuccess ||
				got.Degraded || got.ErrorKind != "" ||
				len(got.Items) != 2 ||
				got.Items[0].Rule.Chunk.ChunkID != 1 ||
				got.Items[1].Rule.Chunk.ChunkID != 2 {
				t.Fatalf("fallback result=%+v error=%v", got, err)
			}
			wantKind := "invalid_rerank"
			if mode == "dependency" {
				wantKind = "dependency"
			}
			if mode == "timeout" {
				wantKind = "timeout"
			}
			if got.Stats.Rerank.Status != retrieval.HybridStageFallback ||
				got.Stats.Rerank.ErrorKind != wantKind {
				t.Fatalf("fallback report=%+v", got.Stats.Rerank)
			}
			if got.Items[0].Rule.Chunk.Title == "plugin-only" ||
				got.Items[0].Rule.ArticleContent == "plugin-only" {
				t.Fatal("plugin input aliases fallback result")
			}
		})
	}
}

func TestRAGService_Cancellation(t *testing.T) {
	for _, stage := range []string{"source", "hydration", "rerank"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			exited := make(chan struct{}, 1)
			es := ragSource(
				[]retrieval.Candidate{ragFusionCandidate(1, 0.8)}, nil,
			)
			hydrator := vrHydrator(ragHydrate)
			var reranker retrieval.Reranker

			switch stage {
			case "source":
				es = vrSearcher(func(
					context.Context, string, retrieval.SearchOptions, time.Time,
				) ([]retrieval.Candidate, error) {
					defer func() { exited <- struct{}{} }()
					cancel()
					return []retrieval.Candidate{ragFusionCandidate(1, 0.8)}, nil
				})
			case "hydration":
				hydrator = vrHydrator(func(
					callCtx context.Context, items []retrieval.Candidate,
					at time.Time,
				) (map[uint64]retrieval.HydratedChunk, error) {
					defer func() { exited <- struct{}{} }()
					rows, _ := ragHydrate(callCtx, items, at)
					cancel()
					return rows, nil
				})
			case "rerank":
				reranker = ragRerankFunc(func(
					context.Context, string, []retrieval.RerankDocument,
				) ([]uint64, error) {
					defer func() { exited <- struct{}{} }()
					cancel()
					return []uint64{1}, nil
				})
			}

			got, err := ragService(t,
				es, ragSource([]retrieval.Candidate{}, nil),
				hydrator, reranker,
			).Retrieve(ctx, "测试", retrieval.HybridOptions{})
			ragTake(t, exited)
			ragFailed(t, got, err, context.Canceled)
			if got.ErrorKind != "canceled" ||
				got.Stats.Rerank.Status == retrieval.HybridStageFallback {
				t.Fatal("parent cancellation was converted into fallback")
			}
		})
	}
}

func TestRAGService_Budgets(t *testing.T) {
	if retrieval.HybridTimeout != 12*time.Second ||
		retrieval.HybridHydrationTimeout != 3*time.Second ||
		retrieval.HybridRerankTimeout != time.Second {
		t.Fatal("service budgets changed")
	}

	t.Run("earlier_parent_deadline", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		wantDeadline, _ := ctx.Deadline()
		exited := make(chan struct{}, 2)

		source := vrSearcher(func(
			callCtx context.Context, _ string,
			_ retrieval.SearchOptions, _ time.Time,
		) ([]retrieval.Candidate, error) {
			defer func() { exited <- struct{}{} }()
			deadline, ok := callCtx.Deadline()
			if !ok || !deadline.Equal(wantDeadline) {
				t.Error("source extended the parent deadline")
			}
			<-callCtx.Done()
			return nil, callCtx.Err()
		})
		got, err := ragService(
			t, source, source, vrHydrator(ragHydrate), nil,
		).Retrieve(ctx, "测试", retrieval.HybridOptions{})
		ragTake(t, exited)
		ragTake(t, exited)
		ragFailed(t, got, err, context.DeadlineExceeded)
	})

	t.Run("overall_deadline", func(t *testing.T) {
		// 先消耗一部分整体预算，再进入来源阶段。
		// 若来源重新获得完整 8 秒，就会超过整体 12 秒截止。
		var started, clockEntered time.Time
		exited := make(chan struct{}, 2)
		source := vrSearcher(func(
			ctx context.Context, _ string,
			_ retrieval.SearchOptions, _ time.Time,
		) ([]retrieval.Candidate, error) {
			defer func() { exited <- struct{}{} }()
			deadline, ok := ctx.Deadline()
			if !ok || deadline.Before(started.Add(12*time.Second)) ||
				deadline.After(clockEntered.Add(12*time.Second)) {
				t.Error("source did not inherit the overall 12-second deadline")
			}
			<-ctx.Done()
			return nil, ctx.Err()
		})
		service, err := retrieval.NewHybridServiceWithClock(
			source, source, vrHydrator(ragHydrate), nil,
			func() time.Time {
				clockEntered = time.Now()
				timer := time.NewTimer(5 * time.Second)
				defer timer.Stop()
				<-timer.C
				return ragFusionAt()
			},
		)
		if err != nil {
			t.Fatal(err)
		}
		started = time.Now()
		got, err := service.Retrieve(
			context.Background(), "测试", retrieval.HybridOptions{},
		)
		ragTake(t, exited)
		ragTake(t, exited)
		ragFailed(t, got, err, context.DeadlineExceeded)
	})
}

func TestRAGService_Preflight(t *testing.T) {
	var calls atomic.Int32
	source := vrSearcher(func(
		context.Context, string, retrieval.SearchOptions, time.Time,
	) ([]retrieval.Candidate, error) {
		calls.Add(1)
		return []retrieval.Candidate{}, nil
	})
	hydrator := vrHydrator(ragHydrate)
	for _, mode := range []string{"es", "milvus", "hydrator", "clock"} {
		t.Run("missing_"+mode, func(t *testing.T) {
			var es, mv retrieval.CandidateSearcher = source, source
			var hyd retrieval.CurrentHydrator = hydrator
			clock := ragFusionAt
			switch mode {
			case "es":
				es = nil
			case "milvus":
				mv = nil
			case "hydrator":
				hyd = nil
			case "clock":
				clock = nil
			}
			got, err := retrieval.NewHybridServiceWithClock(
				es, mv, hyd, nil, clock,
			)
			if got != nil || !errors.Is(err, retrieval.ErrHybridConfiguration) {
				t.Fatal("invalid configuration accepted")
			}
		})
	}

	service := ragService(t, source, source, hydrator, nil)
	got, err := service.Retrieve(
		context.Background(), " ", retrieval.HybridOptions{},
	)
	ragFailed(t, got, err, retrieval.ErrInvalidQuery)

	got, err = service.Retrieve(
		context.Background(), "测试", retrieval.HybridOptions{TopK: 51},
	)
	ragFailed(t, got, err, retrieval.ErrInvalidQuery)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err = service.Retrieve(ctx, "", retrieval.HybridOptions{})
	ragFailed(t, got, err, context.Canceled)

	zeroClock, err := retrieval.NewHybridServiceWithClock(
		source, source, hydrator, nil, func() time.Time { return time.Time{} },
	)
	if err != nil {
		t.Fatal(err)
	}
	got, err = zeroClock.Retrieve(
		context.Background(), "测试", retrieval.HybridOptions{},
	)
	ragFailed(t, got, err, retrieval.ErrHybridConfiguration)

	var missing *retrieval.HybridService
	got, err = missing.Retrieve(
		context.Background(), "测试", retrieval.HybridOptions{},
	)
	ragFailed(t, got, err, retrieval.ErrHybridConfiguration)
	if calls.Load() != 0 {
		t.Fatal("preflight invoked a source")
	}
}

func TestRAGLegacy_Version(t *testing.T) {
	for _, version := range []uint64{0, 21, 22} {
		t.Run("es_"+string(rune('a'+version)), func(t *testing.T) {
			verifyCalls := 0
			service := contractService(t,
				func(
					_ context.Context, _ string,
					opts retrieval.SearchOptions, _ time.Time,
				) ([]retrieval.Candidate, error) {
					if opts.VersionID != version {
						t.Fatal("legacy ES entry lost VersionID")
					}
					return contractCandidates(1), nil
				},
				func(
					context.Context, []retrieval.Candidate, time.Time,
				) (map[uint64]struct{}, error) {
					verifyCalls++
					return map[uint64]struct{}{1: {}}, nil
				},
				ragFusionAt,
			)
			got, err := service.Retrieve(
				context.Background(), "测试",
				retrieval.SearchOptions{VersionID: version},
			)
			if version == 22 {
				if !errors.Is(err, retrieval.ErrInvalidCandidates) ||
					got != nil || verifyCalls != 0 {
					t.Fatal("legacy ES accepted a mismatching version")
				}
			} else if err != nil || len(got) != 1 || verifyCalls != 1 {
				t.Fatalf("legacy ES result=%v error=%v", got, err)
			}
		})

		t.Run("milvus_"+string(rune('a'+version)), func(t *testing.T) {
			hydrateCalls := 0
			service := vrNew(t,
				vrSearcher(func(
					_ context.Context, _ string,
					opts retrieval.SearchOptions, _ time.Time,
				) ([]retrieval.Candidate, error) {
					if opts.VersionID != version {
						t.Fatal("legacy vector entry lost VersionID")
					}
					return vrCandidates(1), nil
				}),
				vrHydrator(func(
					_ context.Context, items []retrieval.Candidate, _ time.Time,
				) (map[uint64]retrieval.HydratedChunk, error) {
					hydrateCalls++
					return vrRows(items), nil
				}),
				time.Second,
			)
			got, err := service.Retrieve(
				context.Background(), "测试",
				retrieval.SearchOptions{VersionID: version},
			)
			if version == 22 {
				if !errors.Is(err, retrieval.ErrInvalidCandidates) ||
					got != nil || hydrateCalls != 0 {
					t.Fatal("legacy vector entry accepted a mismatching version")
				}
			} else if err != nil || len(got) != 1 || hydrateCalls != 1 {
				t.Fatalf("legacy vector result=%v error=%v", got, err)
			}
		})
	}
}
