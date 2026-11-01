package handler

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"video-support-agent/internal/embedding"
	"video-support-agent/internal/middleware"
	"video-support-agent/internal/milvus"
	"video-support-agent/internal/retrieval"

	"github.com/gin-gonic/gin"
)

type vectorHTTPRetriever func(
	context.Context, string, retrieval.SearchOptions,
) ([]retrieval.HydratedChunk, error)

func (f vectorHTTPRetriever) Retrieve(
	ctx context.Context, q string, opts retrieval.SearchOptions,
) ([]retrieval.HydratedChunk, error) {
	return f(ctx, q, opts)
}

const vectorHTTPPath = "/api/v1/knowledge/vector-search"

func vectorHTTPRouter(
	retriever retrieval.HydratedRetriever,
	authenticated bool,
	timeout time.Duration,
) *gin.Engine {
	r := gin.New()
	if timeout > 0 {
		r.Use(middleware.RequestDeadline(timeout))
	}
	if authenticated {
		r.Use(func(c *gin.Context) {
			c.Set("auth.user_id", uint64(99))
			c.Next()
		})
	}
	r.GET(vectorHTTPPath, NewKnowledgeVectorSearchHandler(retriever).Search)
	return r
}

func vectorHTTPRequest(
	r http.Handler, raw string, ctx context.Context,
) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, vectorHTTPPath, nil)
	req.URL.RawQuery = raw
	if ctx != nil {
		req = req.WithContext(ctx)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestVectorHTTPParameters(t *testing.T) {
	valid := []struct {
		name, raw, query string
		opts             retrieval.SearchOptions
	}{
		{"defaults", "q=test", "test", retrieval.SearchOptions{Limit: 10}},
		{
			"zeros", "q=test&document_id=0&category_id=0&limit=0", "test",
			retrieval.SearchOptions{Limit: 10},
		},
		{
			"all_fields",
			"q=" + url.QueryEscape(" 中文  问题 ") +
				"&document_id=11&category_id=31&limit=2",
			"中文  问题",
			retrieval.SearchOptions{DocumentID: 11, CategoryID: 31, Limit: 2},
		},
		{
			"maximum",
			"q=test&document_id=18446744073709551615" +
				"&category_id=18446744073709551615&limit=50",
			"test",
			retrieval.SearchOptions{
				DocumentID: math.MaxUint64, CategoryID: math.MaxUint64, Limit: 50,
			},
		},
		{
			"200_runes", "q=" + url.QueryEscape(strings.Repeat("中", 200)),
			strings.Repeat("中", 200), retrieval.SearchOptions{Limit: 10},
		},
		{
			"raw_8192_bytes", "q=" + strings.Repeat("+", 8189) + "x",
			"x", retrieval.SearchOptions{Limit: 10},
		},
		{"encoded_plus", "q=A%2BB", "A+B", retrieval.SearchOptions{Limit: 10}},
	}
	for _, tc := range valid {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			r := vectorHTTPRouter(vectorHTTPRetriever(func(
				ctx context.Context, q string, opts retrieval.SearchOptions,
			) ([]retrieval.HydratedChunk, error) {
				calls++
				if q != tc.query || opts != tc.opts {
					t.Fatalf("query=%q opts=%+v", q, opts)
				}
				return []retrieval.HydratedChunk{}, nil
			}), true, 0)
			w := vectorHTTPRequest(r, tc.raw, nil)
			searchHTTPWantJSON(t, w, 200,
				fmt.Sprintf(`{"items":[],"count":0,"limit":%d}`, tc.opts.Limit))
			if calls != 1 {
				t.Fatalf("retrieval calls=%d", calls)
			}
		})
	}

	invalid := []string{
		"", "q=", "q=+++", "document_id=1",
		"q=%", "q=%FF", "q=x;y=z",
		"q=x&q=y", "q=x&limit=1&limit=2",
		"q=x&document_id=1&document_id=2",
		"q=x&category_id=1&category_id=2",
		"q=x&section=", "q=x&article_number=", "q=x&highlight=false",
		"q=x&unknown=", "q=x&limit=51", "q=x&limit=-1",
		"q=x&limit=01", "q=x&limit=", "q=x&limit=%2B1",
		"q=x&document_id=18446744073709551616",
		"q=x&document_id=01", "q=x&category_id=-1",
		"q=" + url.QueryEscape(strings.Repeat("中", 201)),
		"q=" + strings.Repeat("+", 8190) + "x",
	}
	for i, raw := range invalid {
		t.Run(fmt.Sprintf("invalid_%02d", i), func(t *testing.T) {
			calls := 0
			r := vectorHTTPRouter(vectorHTTPRetriever(func(
				context.Context, string, retrieval.SearchOptions,
			) ([]retrieval.HydratedChunk, error) {
				calls++
				return []retrieval.HydratedChunk{}, nil
			}), true, 0)
			w := vectorHTTPRequest(r, raw, nil)
			searchHTTPWantError(t, w, 400,
				"invalid_search_query", "invalid search query")
			if calls != 0 {
				t.Fatal("invalid parameters reached retriever")
			}
		})
	}

	t.Run("unauthenticated", func(t *testing.T) {
		w := vectorHTTPRequest(vectorHTTPRouter(nil, false, 0), "q=test", nil)
		searchHTTPWantError(t, w, 401, "unauthorized", "authentication required")
		if w.Header().Get("WWW-Authenticate") != "Bearer" {
			t.Fatal("missing authentication challenge")
		}
	})
}

func TestVectorHTTPDTO(t *testing.T) {
	at := time.Date(2026, 11, 1, 6, 0, 0, 123000000, time.UTC)
	item := retrieval.HydratedChunk{
		Chunk: retrieval.RuleChunk{
			ChunkID: math.MaxUint64, DocumentID: 11, VersionID: 21,
			ArticleID: 41, CategoryID: 31, ChunkIndex: 2, ArticleOrder: 3,
			Version: "v1", Title: "虚构规则", SectionPath: "章节",
			ArticleNumber: "第一条", Content: "检索片段",
			SourceURL:   "https://example.invalid/rule",
			EffectiveAt: at, Score: 0.75,
			Highlights: map[string][]string{"content": {"不应透传"}},
		},
		ArticleContent: "完整条款与片段不同",
	}
	r := vectorHTTPRouter(vectorHTTPRetriever(func(
		context.Context, string, retrieval.SearchOptions,
	) ([]retrieval.HydratedChunk, error) {
		return []retrieval.HydratedChunk{item}, nil
	}), true, 0)
	w := vectorHTTPRequest(r, "q=test&limit=2", nil)
	searchHTTPWantJSON(t, w, 200, `{
		"items":[{
			"chunk_id":18446744073709551615,
			"document_id":11,"version_id":21,"article_id":41,"category_id":31,
			"chunk_index":2,"article_order":3,
			"version":"v1","title":"虚构规则","section_path":"章节",
			"article_number":"第一条","content":"检索片段",
			"source_url":"https://example.invalid/rule",
			"effective_at":"2026-11-01T06:00:00.123Z","expires_at":null,
			"score":0.75,"highlights":{},
			"article_content":"完整条款与片段不同"
		}],
		"count":1,"limit":2
	}`)
	if len(item.Chunk.Highlights) != 1 {
		t.Fatal("handler mutated retriever-owned highlights")
	}

	for _, mode := range []string{"nil", "too_many", "nan", "bad_time", "unconfigured"} {
		t.Run(mode, func(t *testing.T) {
			var dependency retrieval.HydratedRetriever
			if mode != "unconfigured" {
				dependency = vectorHTTPRetriever(func(
					context.Context, string, retrieval.SearchOptions,
				) ([]retrieval.HydratedChunk, error) {
					copyItem := item
					switch mode {
					case "nil":
						return nil, nil
					case "too_many":
						return []retrieval.HydratedChunk{item, item}, nil
					case "nan":
						copyItem.Chunk.Score = math.NaN()
					case "bad_time":
						copyItem.Chunk.EffectiveAt =
							time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
					}
					return []retrieval.HydratedChunk{copyItem}, nil
				})
			}
			w := vectorHTTPRequest(
				vectorHTTPRouter(dependency, true, 0), "q=test&limit=1", nil)
			// Exact single JSON error also proves no partial success JSON was written.
			searchHTTPWantError(t, w, 500, "internal_error", "internal server error")
		})
	}
}

func TestVectorHTTPErrors(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		status  int
		code    string
		message string
	}{
		{"query", retrieval.ErrInvalidQuery, 400,
			"invalid_search_query", "invalid search query"},
		{"input", embedding.ErrInputTooLarge, 400,
			"vector_query_too_large", "query exceeds the embedding input limit"},
		{"context_length", embedding.ErrContextLength, 400,
			"vector_query_too_large", "query exceeds the embedding input limit"},
		{"hydration_limit", errors.Join(retrieval.ErrHydrationFailed, retrieval.ErrHydrationLimit), 503,
			"search_hydration_limit", "rule data exceeds the retrieval read budget"},
		{"hydration", retrieval.ErrHydrationFailed, 503,
			"search_hydration_failed", "rule hydration is temporarily unavailable"},
		{"index", retrieval.ErrIndexNotReady, 503,
			"search_index_not_ready", "search index is not ready"},
		{"model", embedding.ErrModelMismatch, 503,
			"vector_model_mismatch", "embedding model does not match the required contract"},
		{"rate", &milvus.APIError{Code: 1807}, 503,
			"vector_backend_rate_limited", "vector backend is temporarily rate limited"},
		{"embedding_unavailable", embedding.ErrUnavailable, 503,
			"vector_search_unavailable", "vector search dependency is temporarily unavailable"},
		{"milvus_unavailable", milvus.ErrUnavailable, 503,
			"vector_search_unavailable", "vector search dependency is temporarily unavailable"},
		{"embedding_response", embedding.ErrResponse, 502,
			"invalid_vector_backend_response", "vector search dependency returned an invalid response"},
		{"milvus_response", milvus.ErrResponse, 502,
			"invalid_vector_backend_response", "vector search dependency returned an invalid response"},
		{"embedding_rejected", embedding.ErrRejected, 502,
			"vector_backend_rejected", "vector search dependency rejected the request"},
		{"milvus_rejected", milvus.ErrRejected, 502,
			"vector_backend_rejected", "vector search dependency rejected the request"},
		{"other_api_code", &milvus.APIError{Code: 100}, 502,
			"vector_backend_rejected", "vector search dependency rejected the request"},
		{"timeout", context.DeadlineExceeded, 504,
			"request_timeout", "request timed out"},
		{"internal", retrieval.ErrInvalidCandidates, 500,
			"internal_error", "internal server error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			r := vectorHTTPRouter(vectorHTTPRetriever(func(
				context.Context, string, retrieval.SearchOptions,
			) ([]retrieval.HydratedChunk, error) {
				calls++
				return []retrieval.HydratedChunk{{ArticleContent: searchHTTPSecret}},
					fmt.Errorf("%s: %w", searchHTTPSecret, tc.err)
			}), true, 0)
			w := vectorHTTPRequest(r, "q="+searchHTTPSecret, nil)
			searchHTTPWantError(t, w, tc.status, tc.code, tc.message)
			if calls != 1 {
				t.Fatal("dependency retried")
			}
		})
	}
}

func TestVectorHTTPContext(t *testing.T) {
	for _, mode := range []string{
		"cancel_before", "cancel_after", "dependency_cancel",
		"deadline_before", "deadline_during",
	} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			timeout := time.Duration(0)
			if mode == "cancel_before" {
				cancel()
			}
			if mode == "deadline_before" {
				var stop context.CancelFunc
				ctx, stop = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				defer stop()
			}
			if mode == "deadline_during" {
				timeout = 20 * time.Millisecond
			}
			calls := 0
			r := vectorHTTPRouter(vectorHTTPRetriever(func(
				opCtx context.Context, q string, opts retrieval.SearchOptions,
			) ([]retrieval.HydratedChunk, error) {
				calls++
				switch mode {
				case "cancel_after":
					cancel()
					return []retrieval.HydratedChunk{}, errors.New("private dependency detail")
				case "dependency_cancel":
					return nil, context.Canceled
				case "deadline_during":
					if _, ok := opCtx.Deadline(); !ok {
						t.Fatal("request deadline was not propagated")
					}
					<-opCtx.Done()
					return nil, opCtx.Err()
				}
				t.Fatal("already cancelled request reached retriever")
				return nil, nil
			}), true, timeout)

			w := vectorHTTPRequest(r, "q=test", ctx)
			if strings.HasPrefix(mode, "deadline") {
				searchHTTPWantError(t, w, 504, "request_timeout", "request timed out")
			} else if w.Body.Len() != 0 || w.Header().Get("Content-Type") != "" {
				t.Fatal("cancelled request wrote a response body")
			}
			wantCalls := 1
			if mode == "cancel_before" || mode == "deadline_before" {
				wantCalls = 0
			}
			if calls != wantCalls {
				t.Fatalf("calls=%d want=%d", calls, wantCalls)
			}
		})
	}
}
