package retrieval_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"video-support-agent/internal/retrieval"
)

type vrSearcher func(
	context.Context, string, retrieval.SearchOptions, time.Time,
) ([]retrieval.Candidate, error)

func (f vrSearcher) SearchCandidates(
	ctx context.Context, q string, opts retrieval.SearchOptions, at time.Time,
) ([]retrieval.Candidate, error) {
	return f(ctx, q, opts, at)
}

type vrHydrator func(
	context.Context, []retrieval.Candidate, time.Time,
) (map[uint64]retrieval.HydratedChunk, error)

func (f vrHydrator) HydrateCurrent(
	ctx context.Context, candidates []retrieval.Candidate, at time.Time,
) (map[uint64]retrieval.HydratedChunk, error) {
	return f(ctx, candidates, at)
}

func vrNow() time.Time {
	return time.Date(2026, 11, 1, 14, 0, 0, 123456789,
		time.FixedZone("UTC+8", 8*60*60))
}

func vrCandidates(n int) []retrieval.Candidate {
	result := make([]retrieval.Candidate, n)
	at := vrNow().UTC().Truncate(time.Millisecond)
	for i := range result {
		expires := at.Add(24 * time.Hour)
		result[i] = retrieval.Candidate{
			PublicationRevision: 7,
			Chunk: retrieval.RuleChunk{
				ChunkID: uint64(i + 1), DocumentID: 11,
				VersionID: 21, ArticleID: uint64(100 + i),
				CategoryID: 31, Version: "test-v1",
				EffectiveAt: at.Add(-time.Hour), ExpiresAt: &expires,
				Score:      0.9 - float64(i)/300,
				Highlights: make(map[string][]string),
			},
		}
	}
	return result
}

func vrRows(candidates []retrieval.Candidate) map[uint64]retrieval.HydratedChunk {
	result := make(map[uint64]retrieval.HydratedChunk, len(candidates))
	for _, candidate := range candidates {
		chunk := candidate.Chunk
		if chunk.ExpiresAt != nil {
			expires := *chunk.ExpiresAt
			chunk.ExpiresAt = &expires
		}
		chunk.ChunkIndex = 2
		chunk.ArticleOrder = 3
		chunk.Title = "虚构测试规则"
		chunk.SectionPath = "测试章节"
		chunk.ArticleNumber = "测试条款"
		chunk.SourceURL = "https://example.invalid/rule"
		chunk.Content = "测试片段"
		chunk.Score = -0.75 // 必须由候选分数覆盖。
		chunk.Highlights = map[string][]string{"content": {"不应透传"}}
		result[chunk.ChunkID] = retrieval.HydratedChunk{
			Chunk: chunk, ArticleContent: "完整测试条款，包含测试片段和其他内容。",
		}
	}
	return result
}

func vrNew(
	t *testing.T,
	searcher retrieval.CandidateSearcher,
	hydrator retrieval.CurrentHydrator,
	timeout time.Duration,
) *retrieval.MilvusRetriever {
	t.Helper()
	r, err := retrieval.NewMilvusRetrieverWithClock(
		searcher, hydrator, timeout, vrNow,
	)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func vrFail(
	t *testing.T,
	got []retrieval.HydratedChunk,
	err, want error,
) {
	t.Helper()
	if got != nil || err == nil || (want != nil && !errors.Is(err, want)) {
		t.Fatalf("got=%v err=%v want=%v", got, err, want)
	}
}

func TestVectorRetrieverConstruction(t *testing.T) {
	for _, mode := range []string{
		"nil_searcher", "nil_hydrator", "nil_clock",
		"zero_timeout", "negative_timeout", "valid",
	} {
		t.Run(mode, func(t *testing.T) {
			var searcher retrieval.CandidateSearcher = vrSearcher(func(
				context.Context, string, retrieval.SearchOptions, time.Time,
			) ([]retrieval.Candidate, error) {
				t.Fatal("constructor invoked search")
				return nil, nil
			})
			var hydrator retrieval.CurrentHydrator = vrHydrator(func(
				context.Context, []retrieval.Candidate, time.Time,
			) (map[uint64]retrieval.HydratedChunk, error) {
				t.Fatal("constructor invoked hydration")
				return nil, nil
			})
			clock := vrNow
			timeout := time.Second
			switch mode {
			case "nil_searcher":
				searcher = nil
			case "nil_hydrator":
				hydrator = nil
			case "nil_clock":
				clock = nil
			case "zero_timeout":
				timeout = 0
			case "negative_timeout":
				timeout = -time.Second
			}
			got, err := retrieval.NewMilvusRetrieverWithClock(
				searcher, hydrator, timeout, clock,
			)
			if mode == "valid" {
				if got == nil || err != nil {
					t.Fatal("valid constructor failed")
				}
				got, err = retrieval.NewMilvusRetriever(searcher, hydrator, timeout)
				if got == nil || err != nil {
					t.Fatal("default-clock constructor failed")
				}
			} else if got != nil || err == nil {
				t.Fatal("invalid constructor input accepted")
			}
		})
	}
}

func TestVectorRetrieverPreflight(t *testing.T) {
	for _, mode := range []string{
		"empty", "blank", "utf8", "runes", "bytes",
		"section", "article", "highlight", "limit_negative", "limit_large",
		"nil_receiver", "zero_receiver", "zero_clock", "invalid_clock",
	} {
		t.Run(mode, func(t *testing.T) {
			searcher := vrSearcher(func(
				context.Context, string, retrieval.SearchOptions, time.Time,
			) ([]retrieval.Candidate, error) {
				t.Fatal("invalid input reached search")
				return nil, nil
			})
			hydrator := vrHydrator(func(
				context.Context, []retrieval.Candidate, time.Time,
			) (map[uint64]retrieval.HydratedChunk, error) {
				t.Fatal("invalid input reached hydration")
				return nil, nil
			})
			r := vrNew(t, searcher, hydrator, time.Second)
			query := "中文问题"
			opts := retrieval.SearchOptions{}
			want := error(retrieval.ErrInvalidQuery)
			switch mode {
			case "empty":
				query = ""
			case "blank":
				query = " \n"
			case "utf8":
				query = string([]byte{0xff})
			case "runes":
				query = strings.Repeat("中", 201)
			case "bytes":
				query = strings.Repeat(" ", (64<<10)+1)
			case "section":
				opts.Section = " "
			case "article":
				opts.ArticleNumber = "1"
			case "highlight":
				opts.Highlight = true
			case "limit_negative":
				opts.Limit = -1
			case "limit_large":
				opts.Limit = 51
			case "nil_receiver":
				r = nil
				want = nil
			case "zero_receiver":
				r = &retrieval.MilvusRetriever{}
				want = nil
			case "zero_clock", "invalid_clock":
				clock := func() time.Time { return time.Time{} }
				if mode == "invalid_clock" {
					clock = func() time.Time {
						return time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
					}
				}
				var err error
				r, err = retrieval.NewMilvusRetrieverWithClock(
					searcher, hydrator, time.Second, clock,
				)
				if err != nil {
					t.Fatal(err)
				}
				want = nil
			}
			got, err := r.Retrieve(context.Background(), query, opts)
			vrFail(t, got, err, want)
		})
	}
}

func TestVectorRetrieverCandidates(t *testing.T) {
	for _, mode := range []string{
		"nil", "over_budget", "over_maximum", "duplicate",
		"chunk_id", "document_id", "version_id", "article_id", "category_id",
		"blank_version", "utf8_version", "long_version",
		"zero_time", "future", "expired", "expiry_before_effective",
		"nan", "infinity", "score_range", "score_order",
		"document_filter", "category_filter",
	} {
		t.Run(mode, func(t *testing.T) {
			candidates := vrCandidates(2)
			opts := retrieval.SearchOptions{Limit: 1}
			switch mode {
			case "nil":
				candidates = nil
			case "over_budget":
				candidates = vrCandidates(6)
			case "over_maximum":
				candidates = vrCandidates(251)
				opts.Limit = 50
			case "duplicate":
				candidates[1].Chunk.ChunkID = candidates[0].Chunk.ChunkID
			case "chunk_id":
				candidates[0].Chunk.ChunkID = 0
			case "document_id":
				candidates[0].Chunk.DocumentID = 0
			case "version_id":
				candidates[0].Chunk.VersionID = 0
			case "article_id":
				candidates[0].Chunk.ArticleID = 0
			case "category_id":
				candidates[0].Chunk.CategoryID = 0
			case "blank_version":
				candidates[0].Chunk.Version = " "
			case "utf8_version":
				candidates[0].Chunk.Version = string([]byte{0xff})
			case "long_version":
				candidates[0].Chunk.Version = strings.Repeat("x", 257)
			case "zero_time":
				candidates[0].Chunk.EffectiveAt = time.Time{}
			case "future":
				candidates[0].Chunk.EffectiveAt = vrNow().Add(time.Hour)
			case "expired":
				expired := vrNow().UTC().Truncate(time.Millisecond)
				candidates[0].Chunk.ExpiresAt = &expired
			case "expiry_before_effective":
				expires := candidates[0].Chunk.EffectiveAt.Add(-time.Second)
				candidates[0].Chunk.ExpiresAt = &expires
			case "nan":
				candidates[0].Chunk.Score = math.NaN()
			case "infinity":
				candidates[0].Chunk.Score = math.Inf(1)
			case "score_range":
				candidates[0].Chunk.Score = -1.1
			case "score_order":
				candidates[1].Chunk.Score = 0.99
			case "document_filter":
				opts.DocumentID = 999
			case "category_filter":
				opts.CategoryID = 999
			}

			calls := 0
			r := vrNew(t, vrSearcher(func(
				context.Context, string, retrieval.SearchOptions, time.Time,
			) ([]retrieval.Candidate, error) {
				calls++
				return candidates, nil
			}), vrHydrator(func(
				context.Context, []retrieval.Candidate, time.Time,
			) (map[uint64]retrieval.HydratedChunk, error) {
				t.Fatal("invalid candidates reached hydration")
				return nil, nil
			}), time.Second)

			got, err := r.Retrieve(context.Background(), "中文问题", opts)
			vrFail(t, got, err, retrieval.ErrInvalidCandidates)
			if calls != 1 {
				t.Fatal("unexpected search calls")
			}
		})
	}
}

func TestVectorRetrieverFlow(t *testing.T) {
	for _, mode := range []string{
		"ordered_limit", "stale_first", "undersupply",
		"all_stale", "empty_search", "copies", "default_limit",
	} {
		t.Run(mode, func(t *testing.T) {
			candidates := vrCandidates(3)
			for i, id := range []uint64{30, 10, 20} {
				candidates[i].Chunk.ChunkID = id
			}
			rows := vrRows(candidates)
			wantIDs := []uint64{30, 10}
			limit := 2
			switch mode {
			case "stale_first":
				delete(rows, 30)
				wantIDs = []uint64{10, 20}
			case "undersupply":
				delete(rows, 30)
				delete(rows, 20)
				wantIDs = []uint64{10}
			case "all_stale":
				rows = map[uint64]retrieval.HydratedChunk{}
				wantIDs = []uint64{}
			case "empty_search":
				candidates = []retrieval.Candidate{}
				wantIDs = []uint64{}
			case "default_limit":
				limit = 0
				wantIDs = []uint64{30, 10, 20}
			}

			expected := make(map[uint64]retrieval.HydratedChunk)
			for _, candidate := range candidates {
				item, exists := rows[candidate.Chunk.ChunkID]
				if !exists {
					continue
				}
				if item.Chunk.ExpiresAt != nil {
					expires := *item.Chunk.ExpiresAt
					item.Chunk.ExpiresAt = &expires
				}
				item.Chunk.Score = candidate.Chunk.Score
				item.Chunk.Highlights = map[string][]string{}
				expected[candidate.Chunk.ChunkID] = item
			}

			searchCalls, hydrationCalls := 0, 0
			var searchCtx context.Context
			r := vrNew(t, vrSearcher(func(
				ctx context.Context, query string, opts retrieval.SearchOptions, at time.Time,
			) ([]retrieval.Candidate, error) {
				searchCalls++
				searchCtx = ctx
				wantLimit := limit
				if wantLimit == 0 {
					wantLimit = 10
				}
				if query != "中文  问题" ||
					opts != (retrieval.SearchOptions{
						DocumentID: 11, CategoryID: 31, Limit: wantLimit,
					}) ||
					!at.Equal(vrNow().UTC().Truncate(time.Millisecond)) ||
					at.Location() != time.UTC {
					t.Fatal("normalized query/options/time differ")
				}
				return candidates, nil
			}), vrHydrator(func(
				ctx context.Context, input []retrieval.Candidate, at time.Time,
			) (map[uint64]retrieval.HydratedChunk, error) {
				hydrationCalls++
				if ctx != searchCtx ||
					!at.Equal(vrNow().UTC().Truncate(time.Millisecond)) ||
					!reflect.DeepEqual(input, candidates) {
					t.Fatal("hydration input or shared context differs")
				}
				if mode == "copies" {
					// 修改原搜索结果和回源参数，不应影响冻结后的排序/校验。
					candidates[0].Chunk.Score = -1
					*candidates[0].Chunk.ExpiresAt = time.Time{}
					input[0].Chunk.ChunkID = 999
					*input[1].Chunk.ExpiresAt = time.Time{}
				}
				return rows, nil
			}), time.Second)

			got, err := r.Retrieve(context.Background(), " 中文  问题 ", retrieval.SearchOptions{
				DocumentID: 11, CategoryID: 31, Limit: limit,
			})
			if err != nil || got == nil || len(got) != len(wantIDs) {
				t.Fatalf("got=%v err=%v", got, err)
			}
			for i, id := range wantIDs {
				if !reflect.DeepEqual(got[i], expected[id]) {
					t.Fatalf("result[%d] differs: %+v", i, got[i])
				}
				if got[i].Chunk.Content == got[i].ArticleContent {
					t.Fatal("fixture failed to distinguish fragment and article")
				}
			}
			wantHydration := 1
			if mode == "empty_search" {
				wantHydration = 0
			}
			if searchCalls != 1 || hydrationCalls != wantHydration {
				t.Fatal("dependencies repeated or empty search hydrated")
			}
			if mode == "copies" {
				before := *rows[30].Chunk.ExpiresAt
				*got[0].Chunk.ExpiresAt = time.Time{}
				got[0].Chunk.Highlights["x"] = []string{"changed"}
				if !rows[30].Chunk.ExpiresAt.Equal(before) ||
					len(rows[30].Chunk.Highlights) != 1 {
					t.Fatal("result aliases hydration storage")
				}
			}
		})
	}
}

func TestVectorRetrieverHydratedResults(t *testing.T) {
	for _, mode := range []string{
		"nil", "extra_id", "key_mismatch", "document", "version_id",
		"article_id", "category", "version", "effective", "expiry_missing",
		"expiry_changed", "article_order", "empty_fragment", "empty_article",
		"utf8", "bad_after_limit",
	} {
		t.Run(mode, func(t *testing.T) {
			candidates := vrCandidates(3)
			rows := vrRows(candidates)
			item := rows[1]
			switch mode {
			case "nil":
				rows = nil
			case "extra_id":
				delete(rows, 3)
				rows[999] = rows[1]
			case "key_mismatch":
				item.Chunk.ChunkID = 2
			case "document":
				item.Chunk.DocumentID++
			case "version_id":
				item.Chunk.VersionID++
			case "article_id":
				item.Chunk.ArticleID++
			case "category":
				item.Chunk.CategoryID++
			case "version":
				item.Chunk.Version = "another"
			case "effective":
				item.Chunk.EffectiveAt = item.Chunk.EffectiveAt.Add(time.Millisecond)
			case "expiry_missing":
				item.Chunk.ExpiresAt = nil
			case "expiry_changed":
				value := item.Chunk.ExpiresAt.Add(time.Millisecond)
				item.Chunk.ExpiresAt = &value
			case "article_order":
				item.Chunk.ArticleOrder = 0
			case "empty_fragment":
				item.Chunk.Content = " "
			case "empty_article":
				item.ArticleContent = ""
			case "utf8":
				item.Chunk.Title = string([]byte{0xff})
			case "bad_after_limit":
				tail := rows[3]
				tail.ArticleContent = ""
				rows[3] = tail
			}
			if rows != nil {
				rows[1] = item
			}
			r := vrNew(t, vrSearcher(func(
				context.Context, string, retrieval.SearchOptions, time.Time,
			) ([]retrieval.Candidate, error) {
				return candidates, nil
			}), vrHydrator(func(
				context.Context, []retrieval.Candidate, time.Time,
			) (map[uint64]retrieval.HydratedChunk, error) {
				return rows, nil
			}), time.Second)

			got, err := r.Retrieve(
				context.Background(), "中文问题", retrieval.SearchOptions{Limit: 1},
			)
			vrFail(t, got, err, retrieval.ErrHydrationFailed)
		})
	}
}

func TestVectorRetrieverBudgets(t *testing.T) {
	for _, mode := range []string{
		"row_exact", "row_over", "total_exact", "total_over",
	} {
		t.Run(mode, func(t *testing.T) {
			n := 1
			if mode == "total_exact" {
				n = 8
			}
			if mode == "total_over" {
				n = 9
			}
			candidates := vrCandidates(n)
			rows := vrRows(candidates)
			for id, item := range rows {
				chunk := item.Chunk
				otherBytes := len(chunk.Version) + len(chunk.Title) +
					len(chunk.SourceURL) + len(chunk.SectionPath) +
					len(chunk.ArticleNumber) + len(chunk.Content)
				size := retrieval.MaxHydrationRowBytes - otherBytes
				if mode == "row_over" {
					size++
				}
				item.ArticleContent = strings.Repeat("x", size)
				rows[id] = item
			}
			r := vrNew(t, vrSearcher(func(
				context.Context, string, retrieval.SearchOptions, time.Time,
			) ([]retrieval.Candidate, error) {
				return candidates, nil
			}), vrHydrator(func(
				context.Context, []retrieval.Candidate, time.Time,
			) (map[uint64]retrieval.HydratedChunk, error) {
				return rows, nil
			}), 5*time.Second)

			got, err := r.Retrieve(
				context.Background(), "中文问题", retrieval.SearchOptions{Limit: 2},
			)
			if strings.HasSuffix(mode, "_over") {
				vrFail(t, got, err, retrieval.ErrHydrationLimit)
				if !errors.Is(err, retrieval.ErrHydrationFailed) {
					t.Fatal("hydration failure classification lost")
				}
			} else if err != nil || len(got) != min(n, 2) {
				t.Fatalf("exact budget rejected: len=%d err=%v", len(got), err)
			}
		})
	}
}

func TestVectorRetrieverDependencyErrors(t *testing.T) {
	for _, stage := range []string{"search", "hydration"} {
		t.Run(stage, func(t *testing.T) {
			sentinel := errors.New("dependency failure")
			candidates := vrCandidates(1)
			rows := vrRows(candidates)
			searchCalls, hydrationCalls := 0, 0
			r := vrNew(t, vrSearcher(func(
				context.Context, string, retrieval.SearchOptions, time.Time,
			) ([]retrieval.Candidate, error) {
				searchCalls++
				if stage == "search" {
					return candidates, fmt.Errorf("wrapped: %w", sentinel)
				}
				return candidates, nil
			}), vrHydrator(func(
				context.Context, []retrieval.Candidate, time.Time,
			) (map[uint64]retrieval.HydratedChunk, error) {
				hydrationCalls++
				return rows, fmt.Errorf("wrapped: %w", sentinel)
			}), time.Second)

			got, err := r.Retrieve(context.Background(), "中文问题", retrieval.SearchOptions{})
			vrFail(t, got, err, sentinel)
			wantHydration := 0
			if stage == "hydration" {
				wantHydration = 1
				if !errors.Is(err, retrieval.ErrHydrationFailed) {
					t.Fatal("hydration error classification lost")
				}
			}
			if searchCalls != 1 || hydrationCalls != wantHydration {
				t.Fatal("failure retried or continued")
			}
		})
	}
}

func TestVectorRetrieverCancellation(t *testing.T) {
	for _, mode := range []string{
		"before", "search_cancel", "hydration_cancel", "shared_timeout", "parent_timeout",
	} {
		t.Run(mode, func(t *testing.T) {
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			want := error(context.Canceled)
			if mode == "before" {
				cancel()
			}
			if mode == "parent_timeout" {
				var timeoutCancel context.CancelFunc
				parent, timeoutCancel = context.WithTimeout(parent, 500*time.Millisecond)
				defer timeoutCancel()
				want = context.DeadlineExceeded
			}
			if mode == "shared_timeout" {
				want = context.DeadlineExceeded
			}
			candidates := vrCandidates(1)
			searchCalls, hydrationCalls := 0, 0
			var searchDeadline time.Time

			r := vrNew(t, vrSearcher(func(
				ctx context.Context, _ string, _ retrieval.SearchOptions, _ time.Time,
			) ([]retrieval.Candidate, error) {
				searchCalls++
				var ok bool
				searchDeadline, ok = ctx.Deadline()
				if !ok {
					t.Fatal("search has no deadline")
				}
				if mode == "search_cancel" {
					cancel()
				}
				return candidates, nil
			}), vrHydrator(func(
				ctx context.Context, _ []retrieval.Candidate, _ time.Time,
			) (map[uint64]retrieval.HydratedChunk, error) {
				hydrationCalls++
				deadline, ok := ctx.Deadline()
				if !ok || !deadline.Equal(searchDeadline) {
					t.Fatal("hydration received a fresh time budget")
				}
				if mode == "hydration_cancel" {
					cancel()
				} else {
					<-ctx.Done()
				}
				// 取消必须优先于依赖返回的普通错误。
				return vrRows(candidates), errors.New("late dependency error")
			}), time.Second)

			got, err := r.Retrieve(parent, "中文问题", retrieval.SearchOptions{})
			vrFail(t, got, err, want)
			wantSearch, wantHydration := 1, 1
			if mode == "before" {
				wantSearch, wantHydration = 0, 0
			} else if mode == "search_cancel" {
				wantHydration = 0
			}
			if searchCalls != wantSearch || hydrationCalls != wantHydration {
				t.Fatal("canceled retrieval continued or retried")
			}
		})
	}
}
