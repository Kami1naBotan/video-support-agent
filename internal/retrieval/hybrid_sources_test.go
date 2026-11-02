package retrieval_test

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"video-support-agent/internal/retrieval"
)

func ragSource(
	items []retrieval.Candidate,
	err error,
) retrieval.CandidateSearcher {
	return vrSearcher(func(
		context.Context, string, retrieval.SearchOptions, time.Time,
	) ([]retrieval.Candidate, error) {
		return items, err
	})
}

func ragCollector(
	t *testing.T,
	es, mv retrieval.CandidateSearcher,
) *retrieval.ParallelCandidates {
	t.Helper()
	collector, err := retrieval.NewParallelCandidates(es, mv)
	if err != nil {
		t.Fatal(err)
	}
	return collector
}

func ragTake[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	timer := time.NewTimer(20 * time.Second)
	defer timer.Stop()
	select {
	case value := <-ch:
		return value
	case <-timer.C:
		t.Fatal("test synchronization timed out")
		var zero T
		return zero
	}
}

// 可在依赖回调所在的 goroutine 使用：失败只记录，不调用 Fatal。
func ragCheckDeadline(
	t *testing.T,
	ctx context.Context,
	before time.Time,
	budget time.Duration,
) {
	t.Helper()
	observed := time.Now()
	deadline, ok := ctx.Deadline()
	if !ok || deadline.Before(before.Add(budget)) ||
		deadline.After(observed.Add(budget)) {
		t.Errorf("deadline=%v present=%v budget=%v", deadline, ok, budget)
	}
}

type ragCollected struct {
	result retrieval.ParallelCandidateResult
	err    error
}

func TestRAGSources_Parallel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	entered := make(chan int, 2)
	release := make(chan struct{})
	finished := make(chan ragCollected, 1)

	makeSource := func(index int) retrieval.CandidateSearcher {
		return vrSearcher(func(
			ctx context.Context, query string,
			opts retrieval.SearchOptions, at time.Time,
		) ([]retrieval.Candidate, error) {
			entered <- index
			select {
			case <-release:
				return []retrieval.Candidate{ragFusionCandidate(1, 0.8)}, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		})
	}

	collector := ragCollector(t, makeSource(0), makeSource(1))
	go func() {
		result, err := collector.Collect(
			ctx, "测试", retrieval.HybridOptions{}, ragFusionAt(),
		)
		finished <- ragCollected{result, err}
	}()

	first := ragTake(t, entered)
	second := ragTake(t, entered)
	if first == second {
		t.Fatal("the same source entered twice")
	}
	close(release)

	got := ragTake(t, finished)
	if got.err != nil || got.result.Degraded || len(got.result.Lists) != 2 {
		t.Fatalf("parallel result=%+v error=%v", got.result, got.err)
	}
	if got.result.Lists[0].Source != retrieval.SourceES ||
		got.result.Lists[1].Source != retrieval.SourceMilvus {
		t.Fatal("completion order changed the fixed source order")
	}
	for _, list := range got.result.Lists {
		if len(list.Candidates) != 1 {
			t.Fatal("source result lost candidates")
		}
		chunk := list.Candidates[0].Chunk
		if chunk.Title != "" || chunk.Content != "" ||
			len(chunk.Highlights) != 0 {
			t.Fatal("index text was not stripped at the source boundary")
		}
	}
}

func TestRAGSources_Matrix(t *testing.T) {
	type sourceCase struct {
		name   string
		items  []retrieval.Candidate
		err    error
		status retrieval.SourceStatus
		kind   retrieval.SourceErrorKind
	}
	cases := []sourceCase{
		{
			name: "success",
			items: []retrieval.Candidate{
				ragFusionCandidate(1, 0.8),
			},
			status: retrieval.SourceSucceeded,
		},
		{
			name: "empty", items: []retrieval.Candidate{},
			status: retrieval.SourceEmpty,
		},
		{
			name:   "invalid_nil",
			status: retrieval.SourceFailed,
			kind:   retrieval.SourceErrorInvalidCandidates,
		},
		{
			name: "dependency", err: errors.New("synthetic dependency failure"),
			status: retrieval.SourceFailed,
			kind:   retrieval.SourceErrorDependency,
		},
		{
			name: "not_ready", err: retrieval.ErrIndexNotReady,
			status: retrieval.SourceFailed,
			kind:   retrieval.SourceErrorIndexNotReady,
		},
		{
			name: "child_timeout", err: context.DeadlineExceeded,
			status: retrieval.SourceFailed,
			kind:   retrieval.SourceErrorTimeout,
		},
		{
			name: "child_canceled", err: context.Canceled,
			status: retrieval.SourceFailed,
			kind:   retrieval.SourceErrorCanceled,
		},
	}

	for _, left := range cases {
		for _, right := range cases {
			t.Run(left.name+"_"+right.name, func(t *testing.T) {
				collector := ragCollector(t,
					ragSource(left.items, left.err),
					ragSource(right.items, right.err),
				)
				got, err := collector.Collect(
					context.Background(), "测试",
					retrieval.HybridOptions{}, ragFusionAt(),
				)

				wantSources := []retrieval.RetrievalSource{}
				for index, tt := range []sourceCase{left, right} {
					report := got.Sources[index]
					source := []retrieval.RetrievalSource{
						retrieval.SourceES, retrieval.SourceMilvus,
					}[index]
					if report.Source != source ||
						report.Status != tt.status ||
						report.ErrorKind != tt.kind ||
						!report.RawCountKnown ||
						report.RawCount != len(tt.items) ||
						report.Duration < 0 {
						t.Fatalf("source report=%+v case=%s", report, tt.name)
					}
					if tt.status == retrieval.SourceSucceeded ||
						tt.status == retrieval.SourceEmpty {
						wantSources = append(wantSources, source)
					}
				}

				if len(wantSources) == 0 {
					if !errors.Is(err, retrieval.ErrHybridUnavailable) ||
						errors.Is(err, context.DeadlineExceeded) ||
						errors.Is(err, context.Canceled) ||
						got.Lists != nil {
						t.Fatalf("unavailable result=%+v error=%v", got, err)
					}
					return
				}

				if err != nil || got.Lists == nil ||
					got.Degraded != (len(wantSources) == 1) {
					t.Fatalf("result=%+v error=%v", got, err)
				}
				actualSources := make([]retrieval.RetrievalSource, 0, 2)
				for _, list := range got.Lists {
					if list.Candidates == nil {
						t.Fatal("successful source returned a nil snapshot")
					}
					actualSources = append(actualSources, list.Source)
				}
				if !reflect.DeepEqual(actualSources, wantSources) {
					t.Fatalf("sources=%v want=%v", actualSources, wantSources)
				}
			})
		}
	}
}

func TestRAGSources_Cancellation(t *testing.T) {
	t.Run("before_start", func(t *testing.T) {
		var calls atomic.Int32
		source := vrSearcher(func(
			context.Context, string, retrieval.SearchOptions, time.Time,
		) ([]retrieval.Candidate, error) {
			calls.Add(1)
			return []retrieval.Candidate{}, nil
		})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		got, err := ragCollector(t, source, source).Collect(
			ctx, "测试", retrieval.HybridOptions{}, ragFusionAt(),
		)
		if !errors.Is(err, context.Canceled) ||
			got.Lists != nil || calls.Load() != 0 {
			t.Fatalf("result=%+v error=%v calls=%d", got, err, calls.Load())
		}
	})

	t.Run("both_running", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		entered := make(chan struct{}, 2)
		exited := make(chan struct{}, 2)
		finished := make(chan ragCollected, 1)

		source := vrSearcher(func(
			ctx context.Context, _ string,
			_ retrieval.SearchOptions, _ time.Time,
		) ([]retrieval.Candidate, error) {
			entered <- struct{}{}
			defer func() { exited <- struct{}{} }()
			<-ctx.Done()
			return nil, ctx.Err()
		})
		collector := ragCollector(t, source, source)
		go func() {
			result, err := collector.Collect(
				ctx, "测试", retrieval.HybridOptions{}, ragFusionAt(),
			)
			finished <- ragCollected{result, err}
		}()

		ragTake(t, entered)
		ragTake(t, entered)
		cancel()
		got := ragTake(t, finished)
		ragTake(t, exited)
		ragTake(t, exited)

		if !errors.Is(got.err, context.Canceled) ||
			got.result.Lists != nil {
			t.Fatalf("canceled result=%+v error=%v", got.result, got.err)
		}
		for _, report := range got.result.Sources {
			if report.Status != retrieval.SourceFailed ||
				report.ErrorKind != retrieval.SourceErrorCanceled {
				t.Fatalf("cancellation report=%+v", report)
			}
		}
	})
}

func TestRAGSources_Timeout(t *testing.T) {
	if retrieval.HybridSourceTimeout != 8*time.Second {
		t.Fatal("source budget changed")
	}
	for _, both := range []bool{false, true} {
		name := "one_timeout"
		if both {
			name = "both_timeout"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			before := time.Now()
			exited := make(chan struct{}, 2)
			blocked := vrSearcher(func(
				ctx context.Context, _ string,
				_ retrieval.SearchOptions, _ time.Time,
			) ([]retrieval.Candidate, error) {
				defer func() { exited <- struct{}{} }()
				ragCheckDeadline(t, ctx, before, 8*time.Second)
				<-ctx.Done()
				return nil, ctx.Err()
			})
			es := ragSource(
				[]retrieval.Candidate{ragFusionCandidate(1, 0.8)}, nil,
			)
			if both {
				es = blocked
			}

			got, err := ragCollector(t, es, blocked).Collect(
				context.Background(), "测试",
				retrieval.HybridOptions{}, ragFusionAt(),
			)
			ragTake(t, exited)
			if both {
				ragTake(t, exited)
				if !errors.Is(err, retrieval.ErrHybridUnavailable) ||
					errors.Is(err, context.DeadlineExceeded) ||
					got.Lists != nil {
					t.Fatalf("double timeout: result=%+v error=%v", got, err)
				}
			} else if err != nil || !got.Degraded ||
				len(got.Lists) != 1 ||
				got.Lists[0].Source != retrieval.SourceES {
				t.Fatalf("single timeout: result=%+v error=%v", got, err)
			}
			for index, report := range got.Sources {
				if index == 0 && !both {
					continue
				}
				if report.Status != retrieval.SourceFailed ||
					report.ErrorKind != retrieval.SourceErrorTimeout {
					t.Fatalf("timeout report=%+v", report)
				}
			}
		})
	}
}

func TestRAGSources_Preflight(t *testing.T) {
	var calls atomic.Int32
	source := vrSearcher(func(
		context.Context, string, retrieval.SearchOptions, time.Time,
	) ([]retrieval.Candidate, error) {
		calls.Add(1)
		return []retrieval.Candidate{}, nil
	})
	for _, deps := range [][2]retrieval.CandidateSearcher{
		{nil, source}, {source, nil},
	} {
		got, err := retrieval.NewParallelCandidates(deps[0], deps[1])
		if err == nil || got != nil {
			t.Fatal("missing dependency accepted")
		}
	}
	collector := ragCollector(t, source, source)
	for _, tt := range []struct {
		query string
		opts  retrieval.HybridOptions
		at    time.Time
	}{
		{"", retrieval.HybridOptions{}, ragFusionAt()},
		{"测试", retrieval.HybridOptions{TopK: 51}, ragFusionAt()},
		{"测试", retrieval.HybridOptions{}, time.Time{}},
	} {
		got, err := collector.Collect(
			context.Background(), tt.query, tt.opts, tt.at,
		)
		if err == nil || got.Lists != nil {
			t.Fatal("invalid input accepted")
		}
	}
	var missing *retrieval.ParallelCandidates
	got, err := missing.Collect(
		context.Background(), "测试",
		retrieval.HybridOptions{}, ragFusionAt(),
	)
	if err == nil || got.Lists != nil || calls.Load() != 0 {
		t.Fatal("preflight reached a dependency")
	}
}
