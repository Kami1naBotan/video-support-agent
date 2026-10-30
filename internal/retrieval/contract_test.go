package retrieval_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"video-support-agent/internal/retrieval"
)

type contractSearcher func(
	context.Context, string, retrieval.SearchOptions, time.Time,
) ([]retrieval.Candidate, error)

func (f contractSearcher) SearchCandidates(
	ctx context.Context,
	query string,
	opts retrieval.SearchOptions,
	at time.Time,
) ([]retrieval.Candidate, error) {
	return f(ctx, query, opts, at)
}

type contractVerifier func(
	context.Context, []retrieval.Candidate, time.Time,
) (map[uint64]struct{}, error)

func (f contractVerifier) VerifyCurrent(
	ctx context.Context,
	candidates []retrieval.Candidate,
	at time.Time,
) (map[uint64]struct{}, error) {
	return f(ctx, candidates, at)
}

func contractCandidates(n int) []retrieval.Candidate {
	items := make([]retrieval.Candidate, n)
	for i := range items {
		items[i] = retrieval.Candidate{
			PublicationRevision: 7,
			Chunk: retrieval.RuleChunk{
				ChunkID:       uint64(i + 1),
				DocumentID:    11,
				VersionID:     21,
				ArticleID:     31,
				CategoryID:    41,
				ChunkIndex:    uint32(i),
				ArticleOrder:  1,
				Version:       "测试版本",
				Title:         "测试规则",
				SectionPath:   "测试章节",
				ArticleNumber: "第一条",
				Content:       "仅用于契约测试的虚构正文",
				SourceURL:     "https://example.invalid/test",
				EffectiveAt:   time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
				Score:         float64(n - i),
				Highlights: map[string][]string{
					"content": {"<em>测试</em>正文"},
				},
			},
		}
	}
	return items
}

func contractService(
	t *testing.T,
	searcher contractSearcher,
	verifier contractVerifier,
	clock func() time.Time,
) *retrieval.Service {
	t.Helper()
	service, err := retrieval.NewServiceWithClock(searcher, verifier, clock)
	if err != nil {
		t.Fatalf("construct service: %v", err)
	}
	return service
}

func TestRetrievalNormalizeContract(t *testing.T) {
	tests := []struct {
		name    string
		query   string
		opts    retrieval.SearchOptions
		wantQ   string
		want    retrieval.SearchOptions
		invalid bool
	}{
		{
			name:  "trim_and_defaults",
			query: "\u3000测试 \t",
			opts: retrieval.SearchOptions{
				DocumentID: 9, CategoryID: 8,
				Section: " 章节 ", ArticleNumber: " 第一条 ",
				Highlight: true,
			},
			wantQ: "测试",
			want: retrieval.SearchOptions{
				DocumentID: 9, CategoryID: 8,
				Section: "章节", ArticleNumber: "第一条",
				Limit: 10, Highlight: true,
			},
		},
		{
			name: "section_only",
			opts: retrieval.SearchOptions{Section: " 章节 "},
			want: retrieval.SearchOptions{Section: "章节", Limit: 10},
		},
		{
			name: "article_only_preserves_number",
			opts: retrieval.SearchOptions{ArticleNumber: " 第〇一条 "},
			want: retrieval.SearchOptions{ArticleNumber: "第〇一条", Limit: 10},
		},
		{
			name:  "query_unicode_boundary",
			query: strings.Repeat("🙂", 200),
			wantQ: strings.Repeat("🙂", 200),
			want:  retrieval.SearchOptions{Limit: 10},
		},
		{
			name: "section_unicode_boundary",
			opts: retrieval.SearchOptions{Section: strings.Repeat("章", 500)},
			want: retrieval.SearchOptions{
				Section: strings.Repeat("章", 500), Limit: 10,
			},
		},
		{
			name: "article_unicode_boundary",
			opts: retrieval.SearchOptions{ArticleNumber: strings.Repeat("条", 64)},
			want: retrieval.SearchOptions{
				ArticleNumber: strings.Repeat("条", 64), Limit: 10,
			},
		},
		{
			name: "minimum_limit", query: "测试",
			opts:  retrieval.SearchOptions{Limit: 1},
			wantQ: "测试", want: retrieval.SearchOptions{Limit: 1},
		},
		{
			name: "maximum_limit", query: "测试",
			opts:  retrieval.SearchOptions{Limit: 50},
			wantQ: "测试", want: retrieval.SearchOptions{Limit: 50},
		},
		{
			name: "internal_whitespace_preserved", query: " 甲  乙 ",
			wantQ: "甲  乙", want: retrieval.SearchOptions{Limit: 10},
		},
		{name: "empty", invalid: true},
		{
			name: "whitespace_only", query: " \t\u3000",
			opts:    retrieval.SearchOptions{Section: "\n", ArticleNumber: " "},
			invalid: true,
		},
		{
			name:    "ids_are_not_text_conditions",
			opts:    retrieval.SearchOptions{DocumentID: 1, CategoryID: 2},
			invalid: true,
		},
		{
			name:  "query_too_long",
			query: strings.Repeat("测", 201), invalid: true,
		},
		{
			name: "section_too_long", query: "测试",
			opts:    retrieval.SearchOptions{Section: strings.Repeat("章", 501)},
			invalid: true,
		},
		{
			name: "article_too_long", query: "测试",
			opts:    retrieval.SearchOptions{ArticleNumber: strings.Repeat("条", 65)},
			invalid: true,
		},
		{
			name:  "query_invalid_utf8",
			query: string([]byte{0xff}), invalid: true,
		},
		{
			name: "section_invalid_utf8", query: "测试",
			opts:    retrieval.SearchOptions{Section: string([]byte{0xff})},
			invalid: true,
		},
		{
			name: "article_invalid_utf8", query: "测试",
			opts:    retrieval.SearchOptions{ArticleNumber: string([]byte{0xff})},
			invalid: true,
		},
		{
			name: "negative_limit", query: "测试",
			opts: retrieval.SearchOptions{Limit: -1}, invalid: true,
		},
		{
			name: "excess_limit", query: "测试",
			opts: retrieval.SearchOptions{Limit: 51}, invalid: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			original := tt.opts
			query, opts, err := retrieval.NormalizeSearch(tt.query, tt.opts)
			if tt.opts != original {
				t.Fatal("input options changed")
			}
			if tt.invalid {
				if !errors.Is(err, retrieval.ErrInvalidQuery) {
					t.Fatalf("want invalid query, got %v", err)
				}
				if query != "" || opts != (retrieval.SearchOptions{}) {
					t.Fatalf("failure returned partial normalized values: %q %+v", query, opts)
				}
				return
			}
			if err != nil || query != tt.wantQ || opts != tt.want {
				t.Fatalf("got (%q, %+v, %v), want (%q, %+v, nil)",
					query, opts, err, tt.wantQ, tt.want)
			}
		})
	}
}

func TestRetrievalOrderingAndSharedTime(t *testing.T) {
	rawTime := time.Date(
		2026, 10, 30, 12, 34, 56, 123456789,
		time.FixedZone("test-zone", 8*60*60),
	)
	wantTime := rawTime.UTC().Truncate(time.Millisecond)
	candidates := contractCandidates(5)
	clockCalls := 0
	var calls []string
	type contextKey struct{}
	ctx := context.WithValue(context.Background(), contextKey{}, "marker")

	wantOpts := retrieval.SearchOptions{
		DocumentID: 11, CategoryID: 41,
		Section: "章节", ArticleNumber: "第一条",
		Limit: 2, Highlight: true,
	}

	service := contractService(t,
		func(gotCtx context.Context, query string, opts retrieval.SearchOptions,
			at time.Time) ([]retrieval.Candidate, error) {
			calls = append(calls, "search")
			if gotCtx.Value(contextKey{}) != "marker" {
				t.Fatal("search lost request context")
			}
			if query != "测试" || opts != wantOpts || at != wantTime {
				t.Fatalf("incorrect search arguments: %q %+v %v", query, opts, at)
			}
			return candidates, nil
		},
		func(gotCtx context.Context, got []retrieval.Candidate,
			at time.Time) (map[uint64]struct{}, error) {
			calls = append(calls, "verify")
			if gotCtx.Value(contextKey{}) != "marker" || at != wantTime {
				t.Fatal("verification lost context or shared query time")
			}
			if !reflect.DeepEqual(got, candidates) {
				t.Fatal("verification must receive all candidates unchanged")
			}
			return map[uint64]struct{}{3: {}, 4: {}, 5: {}}, nil
		},
		func() time.Time {
			clockCalls++
			calls = append(calls, "clock")
			return rawTime
		},
	)

	opts := wantOpts
	opts.Section = " 章节 "
	opts.ArticleNumber = " 第一条 "
	got, err := service.Retrieve(ctx, " 测试 ", opts)
	if err != nil {
		t.Fatal(err)
	}
	if clockCalls != 1 || !reflect.DeepEqual(calls, []string{"clock", "search", "verify"}) {
		t.Fatalf("incorrect call sequence: %v, clock calls=%d", calls, clockCalls)
	}
	want := []retrieval.RuleChunk{candidates[2].Chunk, candidates[3].Chunk}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("must preserve candidate order after verification: %+v", got)
	}
}

func TestRetrievalResultIsolation(t *testing.T) {
	for _, highlight := range []bool{false, true} {
		name := "disabled"
		if highlight {
			name = "enabled"
		}
		t.Run(name, func(t *testing.T) {
			candidates := contractCandidates(1)
			expiry := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
			candidates[0].Chunk.ExpiresAt = &expiry
			originalExpiry := expiry

			service := contractService(t,
				func(context.Context, string, retrieval.SearchOptions,
					time.Time) ([]retrieval.Candidate, error) {
					return candidates, nil
				},
				func(context.Context, []retrieval.Candidate,
					time.Time) (map[uint64]struct{}, error) {
					return map[uint64]struct{}{1: {}}, nil
				},
				func() time.Time { return originalExpiry.Add(-time.Hour) },
			)

			got, err := service.Retrieve(context.Background(), "测试",
				retrieval.SearchOptions{Highlight: highlight})
			if err != nil || len(got) != 1 {
				t.Fatalf("got %+v, %v", got, err)
			}
			if got[0].Highlights == nil || got[0].ExpiresAt == nil {
				t.Fatal("expected non-nil highlights and copied expiry")
			}
			if highlight {
				if !reflect.DeepEqual(got[0], candidates[0].Chunk) {
					t.Fatal("returned chunk lost source fields")
				}
				got[0].Highlights["content"][0] = "changed fragment"
			} else if len(got[0].Highlights) != 0 {
				t.Fatal("disabled highlights must be empty")
			}

			got[0].Highlights["title"] = []string{"new field"}
			*got[0].ExpiresAt = originalExpiry.Add(time.Hour)

			if expiry != originalExpiry {
				t.Fatal("expiry pointer aliases candidate memory")
			}
			if candidates[0].Chunk.Highlights["content"][0] != "<em>测试</em>正文" {
				t.Fatal("highlight slice aliases candidate memory")
			}
			if _, exists := candidates[0].Chunk.Highlights["title"]; exists {
				t.Fatal("highlight map aliases candidate memory")
			}
		})
	}
}

func TestRetrievalEmptyAndShortResults(t *testing.T) {
	tests := []struct {
		name       string
		candidates []retrieval.Candidate
		accepted   map[uint64]struct{}
		wantIDs    []uint64
		wantVerify int
	}{
		{"no_candidates", []retrieval.Candidate{}, nil, []uint64{}, 0},
		{"all_stale", contractCandidates(3), map[uint64]struct{}{}, []uint64{}, 1},
		{"short_result", contractCandidates(3), map[uint64]struct{}{3: {}}, []uint64{3}, 1},
		{"maximum_budget", contractCandidates(250), map[uint64]struct{}{250: {}}, []uint64{250}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			searchCalls, verifyCalls := 0, 0
			service := contractService(t,
				func(context.Context, string, retrieval.SearchOptions,
					time.Time) ([]retrieval.Candidate, error) {
					searchCalls++
					return tt.candidates, nil
				},
				func(context.Context, []retrieval.Candidate,
					time.Time) (map[uint64]struct{}, error) {
					verifyCalls++
					return tt.accepted, nil
				},
				func() time.Time { return time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC) },
			)
			got, err := service.Retrieve(context.Background(), "测试",
				retrieval.SearchOptions{Limit: 50})
			if err != nil || got == nil {
				t.Fatalf("expected non-nil success result: %+v, %v", got, err)
			}
			ids := make([]uint64, 0, len(got))
			for _, item := range got {
				ids = append(ids, item.ChunkID)
				if item.Highlights == nil || len(item.Highlights) != 0 {
					t.Fatal("default highlights must be an empty map")
				}
				if item.ExpiresAt != nil {
					t.Fatal("nil expiry must remain nil")
				}
			}
			if !reflect.DeepEqual(ids, tt.wantIDs) ||
				searchCalls != 1 || verifyCalls != tt.wantVerify {
				t.Fatalf("ids=%v, search calls=%d, verify calls=%d",
					ids, searchCalls, verifyCalls)
			}
		})
	}
}

func TestRetrievalDependencyFailures(t *testing.T) {
	searchFailure := errors.New("search failure")
	verifyFailure := errors.New("verification failure")
	duplicate := contractCandidates(2)
	duplicate[1].Chunk.ChunkID = duplicate[0].Chunk.ChunkID
	zeroID := contractCandidates(1)
	zeroID[0].Chunk.ChunkID = 0

	tests := []struct {
		name       string
		candidates []retrieval.Candidate
		searchErr  error
		accepted   map[uint64]struct{}
		verifyErr  error
		want       error
		wantVerify int
	}{
		{
			name:       "search_error_discards_partial",
			candidates: contractCandidates(1), searchErr: searchFailure,
			want: searchFailure,
		},
		{
			name:      "index_not_ready",
			searchErr: retrieval.ErrIndexNotReady, want: retrieval.ErrIndexNotReady,
		},
		{
			name:      "search_deadline",
			searchErr: context.DeadlineExceeded, want: context.DeadlineExceeded,
		},
		{
			name: "nil_candidates", want: retrieval.ErrInvalidCandidates,
		},
		{
			name: "per_query_budget_exceeded", candidates: contractCandidates(6),
			want: retrieval.ErrInvalidCandidates,
		},
		{
			name: "zero_id", candidates: zeroID, want: retrieval.ErrInvalidCandidates,
		},
		{
			name: "duplicate_id", candidates: duplicate, want: retrieval.ErrInvalidCandidates,
		},
		{
			name:       "verification_error_discards_partial",
			candidates: contractCandidates(1),
			accepted:   map[uint64]struct{}{1: {}}, verifyErr: verifyFailure,
			want: retrieval.ErrVerificationFailed, wantVerify: 1,
		},
		{
			name: "nil_accepted_set", candidates: contractCandidates(1),
			want: retrieval.ErrVerificationFailed, wantVerify: 1,
		},
		{
			name: "foreign_accepted_id", candidates: contractCandidates(1),
			accepted: map[uint64]struct{}{1: {}, 999: {}},
			want:     retrieval.ErrVerificationFailed, wantVerify: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			searchCalls, verifyCalls := 0, 0
			service := contractService(t,
				func(context.Context, string, retrieval.SearchOptions,
					time.Time) ([]retrieval.Candidate, error) {
					searchCalls++
					return tt.candidates, tt.searchErr
				},
				func(context.Context, []retrieval.Candidate,
					time.Time) (map[uint64]struct{}, error) {
					verifyCalls++
					return tt.accepted, tt.verifyErr
				},
				func() time.Time { return time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC) },
			)
			got, err := service.Retrieve(context.Background(), "测试",
				retrieval.SearchOptions{Limit: 1})
			if got != nil || !errors.Is(err, tt.want) {
				t.Fatalf("must fail without partial results: got=%+v, err=%v", got, err)
			}
			if tt.verifyErr != nil && !errors.Is(err, tt.verifyErr) {
				t.Fatalf("lost original verification error: %v", err)
			}
			if searchCalls != 1 || verifyCalls != tt.wantVerify {
				t.Fatalf("search calls=%d, verify calls=%d", searchCalls, verifyCalls)
			}
		})
	}
}

func TestRetrievalCancellation(t *testing.T) {
	for _, phase := range []string{"before", "clock", "search", "verify"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			clockCalls, searchCalls, verifyCalls := 0, 0, 0
			if phase == "before" {
				cancel()
			}
			service := contractService(t,
				func(context.Context, string, retrieval.SearchOptions,
					time.Time) ([]retrieval.Candidate, error) {
					searchCalls++
					if phase == "search" {
						cancel()
					}
					return contractCandidates(1), nil
				},
				func(context.Context, []retrieval.Candidate,
					time.Time) (map[uint64]struct{}, error) {
					verifyCalls++
					if phase == "verify" {
						cancel()
					}
					return map[uint64]struct{}{1: {}}, nil
				},
				func() time.Time {
					clockCalls++
					if phase == "clock" {
						cancel()
					}
					return time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
				},
			)
			got, err := service.Retrieve(ctx, "测试", retrieval.SearchOptions{})
			if got != nil || !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation must discard results: %+v, %v", got, err)
			}
			want := map[string][3]int{
				"before": {0, 0, 0},
				"clock":  {1, 0, 0},
				"search": {1, 1, 0},
				"verify": {1, 1, 1},
			}[phase]
			if actual := [3]int{clockCalls, searchCalls, verifyCalls}; actual != want {
				t.Fatalf("calls=%v, want=%v", actual, want)
			}
		})
	}
}

func TestRetrievalConfigurationAndEarlyRejection(t *testing.T) {
	searcher := contractSearcher(func(
		context.Context, string, retrieval.SearchOptions, time.Time,
	) ([]retrieval.Candidate, error) {
		t.Fatal("search must not run")
		return nil, nil
	})
	verifier := contractVerifier(func(
		context.Context, []retrieval.Candidate, time.Time,
	) (map[uint64]struct{}, error) {
		t.Fatal("verification must not run")
		return nil, nil
	})
	clock := func() time.Time {
		return time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	}

	t.Run("constructor_dependencies", func(t *testing.T) {
		check := func(s *retrieval.Service, err error) {
			t.Helper()
			if s != nil || err == nil {
				t.Fatalf("invalid construction returned service=%v, err=%v", s, err)
			}
		}
		check(retrieval.NewServiceWithClock(nil, verifier, clock))
		check(retrieval.NewServiceWithClock(searcher, nil, clock))
		check(retrieval.NewServiceWithClock(searcher, verifier, nil))
		s, err := retrieval.NewService(searcher, verifier)
		if err != nil || s == nil {
			t.Fatalf("valid default-clock constructor: %v", err)
		}
	})

	t.Run("unconfigured_service", func(t *testing.T) {
		for _, service := range []*retrieval.Service{nil, new(retrieval.Service)} {
			got, err := service.Retrieve(context.Background(), "测试", retrieval.SearchOptions{})
			if got != nil || err == nil {
				t.Fatalf("unconfigured service returned %+v, %v", got, err)
			}
		}
	})

	t.Run("invalid_input_before_clock", func(t *testing.T) {
		service := contractService(t, searcher, verifier, func() time.Time {
			t.Fatal("invalid input must be rejected before clock")
			return clock()
		})
		got, err := service.Retrieve(context.Background(), "", retrieval.SearchOptions{})
		if got != nil || !errors.Is(err, retrieval.ErrInvalidQuery) {
			t.Fatalf("got %+v, %v", got, err)
		}
	})

	t.Run("expired_context", func(t *testing.T) {
		ctx, cancel := context.WithDeadline(
			context.Background(), time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC),
		)
		defer cancel()
		service := contractService(t, searcher, verifier, func() time.Time {
			t.Fatal("expired request must not invoke clock")
			return clock()
		})
		got, err := service.Retrieve(ctx, "测试", retrieval.SearchOptions{})
		if got != nil || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("got %+v, %v", got, err)
		}
	})

	for _, tt := range []struct {
		name string
		at   time.Time
	}{
		{"zero_clock", time.Time{}},
		{"below_supported_year", time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC)},
		{"above_supported_year", time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			service := contractService(t, searcher, verifier, func() time.Time {
				return tt.at
			})
			got, err := service.Retrieve(context.Background(), "测试", retrieval.SearchOptions{})
			if got != nil || err == nil {
				t.Fatalf("invalid clock returned %+v, %v", got, err)
			}
		})
	}
}
