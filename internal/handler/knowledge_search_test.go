package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"video-support-agent/internal/elasticsearch"
	"video-support-agent/internal/middleware"
	"video-support-agent/internal/retrieval"

	"github.com/gin-gonic/gin"
)

type searchHTTPRetriever func(
	context.Context, string, retrieval.SearchOptions,
) ([]retrieval.RuleChunk, error)

func (f searchHTTPRetriever) Retrieve(
	ctx context.Context, query string, opts retrieval.SearchOptions,
) ([]retrieval.RuleChunk, error) {
	return f(ctx, query, opts)
}

const searchHTTPPath = "/api/v1/knowledge/search"
const searchHTTPSecret = "private-query-and-backend-detail"

func searchHTTPRouter(
	retriever retrieval.Retriever,
	timeout time.Duration,
) *gin.Engine {
	r := gin.New()
	if timeout > 0 {
		r.Use(middleware.RequestDeadline(timeout))
	}
	r.Use(func(c *gin.Context) {
		// 仅用于隔离 Handler，不构成 JWT 或数据库权限证据。
		c.Set("auth.user_id", uint64(99))
		c.Next()
	})
	r.GET(searchHTTPPath, NewKnowledgeSearchHandler(retriever).Search)
	return r
}

func searchHTTPRequest(
	r http.Handler, raw string, ctx context.Context,
) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, searchHTTPPath, nil)
	// 直接设置 RawQuery，以便把非法百分号编码交给被测解析器。
	req.URL.RawQuery = raw
	if ctx != nil {
		req = req.WithContext(ctx)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func searchHTTPJSON(t *testing.T, data []byte) any {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	// 避免 uint64 经 float64 解码而失去精度。
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, data)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		t.Fatalf("unexpected trailing JSON: %v", err)
	}
	return value
}

func searchHTTPWantJSON(
	t *testing.T, w *httptest.ResponseRecorder, status int, expected string,
) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status=%d, want=%d, body=%s", w.Code, status, w.Body.String())
	}
	if !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("unexpected Content-Type: %q", w.Header().Get("Content-Type"))
	}
	got := searchHTTPJSON(t, w.Body.Bytes())
	want := searchHTTPJSON(t, []byte(expected))
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("JSON mismatch\ngot: %s\nwant: %s", w.Body.String(), expected)
	}
}

func searchHTTPWantError(
	t *testing.T, w *httptest.ResponseRecorder,
	status int, code, message string,
) {
	t.Helper()
	body, err := json.Marshal(map[string]string{
		"code": code, "message": message,
	})
	if err != nil {
		t.Fatal(err)
	}
	searchHTTPWantJSON(t, w, status, string(body))
	if strings.Contains(w.Body.String(), searchHTTPSecret) {
		t.Fatal("private query or backend details leaked")
	}
}

func TestKnowledgeSearchHTTPValidParameters(t *testing.T) {
	cases := []struct {
		name  string
		raw   string
		query string
		opts  retrieval.SearchOptions
	}{
		{
			"defaults", "q=test", "test",
			retrieval.SearchOptions{Limit: 10},
		},
		{
			"explicit_zero",
			"q=test&document_id=0&category_id=0&limit=0&highlight=false",
			"test", retrieval.SearchOptions{Limit: 10},
		},
		{
			"all_fields",
			url.Values{
				"q":              {" 测试正文 "},
				"document_id":    {"11"},
				"category_id":    {"41"},
				"article_number": {" 第一条 "},
				"section":        {" 测试章节 "},
				"limit":          {"2"},
				"highlight":      {"true"},
			}.Encode(),
			"测试正文",
			retrieval.SearchOptions{
				DocumentID: 11, CategoryID: 41,
				ArticleNumber: "第一条", Section: "测试章节",
				Limit: 2, Highlight: true,
			},
		},
		{
			"section_only", "section=" + url.QueryEscape("章节"), "",
			retrieval.SearchOptions{Section: "章节", Limit: 10},
		},
		{
			"article_only", "article_number=" + url.QueryEscape("第〇一条"), "",
			retrieval.SearchOptions{ArticleNumber: "第〇一条", Limit: 10},
		},
		{
			"maximum_ids_and_limit",
			"q=test&document_id=18446744073709551615&category_id=18446744073709551615&limit=50",
			"test",
			retrieval.SearchOptions{
				DocumentID: ^uint64(0), CategoryID: ^uint64(0), Limit: 50,
			},
		},
		{
			"minimum_limit", "q=test&limit=1", "test",
			retrieval.SearchOptions{Limit: 1},
		},
		{
			"encoded_plus", "q=A%2BB", "A+B",
			retrieval.SearchOptions{Limit: 10},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			r := searchHTTPRouter(searchHTTPRetriever(func(
				_ context.Context, query string, opts retrieval.SearchOptions,
			) ([]retrieval.RuleChunk, error) {
				calls++
				if query != tc.query || opts != tc.opts {
					t.Fatalf("query=%q opts=%+v, want %q %+v",
						query, opts, tc.query, tc.opts)
				}
				return []retrieval.RuleChunk{}, nil
			}), 0)

			w := searchHTTPRequest(r, tc.raw, nil)
			searchHTTPWantJSON(t, w, 200,
				fmt.Sprintf(`{"items":[],"count":0,"limit":%d}`, tc.opts.Limit))
			if calls != 1 {
				t.Fatalf("retriever calls=%d, want=1", calls)
			}
		})
	}
}

func TestKnowledgeSearchHTTPRejectsParameters(t *testing.T) {
	cases := map[string]string{
		"empty":               "",
		"whitespace_only":     "q=+",
		"ids_only":            "document_id=11&category_id=41",
		"unknown":             "q=test&unknown=value",
		"case_sensitive_name": "Q=test",
		"bad_escape":          "q=%ZZ",
		"semicolon":           "q=test;other=value",
		"utf8_query":          "q=%FF",
		"utf8_section":        "q=test&section=%FF",
		"utf8_article":        "q=test&article_number=%FF",
		"long_query":          "q=" + url.QueryEscape(strings.Repeat("测", 201)),
		"long_section":        "section=" + url.QueryEscape(strings.Repeat("章", 501)),
		"long_article":        "article_number=" + url.QueryEscape(strings.Repeat("条", 65)),
		"limit_too_large":     "q=test&limit=51",
		"encoded_duplicate":   "q=test&%71=second",
	}
	for _, name := range []string{
		"q", "document_id", "category_id", "article_number",
		"section", "limit", "highlight",
	} {
		cases["duplicate_"+name] = "q=test&" + name + "=1&" + name + "=1"
	}
	for _, name := range []string{"document_id", "category_id", "limit"} {
		for i, value := range []string{
			"", "-1", "+1", "01", "1.0", "1e1", " 1", "1 ",
			"18446744073709551616",
		} {
			cases[fmt.Sprintf("%s_bad_%d", name, i)] =
				"q=test&" + name + "=" + url.QueryEscape(value)
		}
	}
	for i, value := range []string{"", "True", "FALSE", "1", "0", " true "} {
		cases[fmt.Sprintf("highlight_bad_%d", i)] =
			"q=test&highlight=" + url.QueryEscape(value)
	}

	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			calls := 0
			r := searchHTTPRouter(searchHTTPRetriever(func(
				context.Context, string, retrieval.SearchOptions,
			) ([]retrieval.RuleChunk, error) {
				calls++
				return []retrieval.RuleChunk{}, nil
			}), 0)

			w := searchHTTPRequest(r, raw, nil)
			searchHTTPWantError(t, w, 400,
				"invalid_search_query", "invalid search query")
			if calls != 0 {
				t.Fatal("invalid input reached retriever")
			}
		})
	}
}

func TestKnowledgeSearchHTTPResponseDTO(t *testing.T) {
	at := time.Date(2026, 10, 30, 4, 0, 0, 123000000, time.UTC)
	expiry := at.Add(time.Hour)
	items := []retrieval.RuleChunk{
		{
			ChunkID: ^uint64(0), DocumentID: 11, VersionID: 21,
			ArticleID: 31, CategoryID: 41,
			ChunkIndex: 2, ArticleOrder: 3,
			Version: "测试版本", Title: "测试标题",
			SectionPath: "测试章节", ArticleNumber: "第一条",
			Content: "测试正文", SourceURL: "https://example.invalid/test",
			EffectiveAt: at, ExpiresAt: &expiry, Score: 2.5,
			Highlights: map[string][]string{
				"content": {"&lt;文本&gt;<em>测试</em>"},
			},
		},
		{
			ChunkID: 2, DocumentID: 12, VersionID: 22,
			ArticleID: 32, CategoryID: 42,
			ChunkIndex: 0, ArticleOrder: 1,
			Version: "另一版本", Title: "另一标题",
			Content: "另一正文", SourceURL: "https://example.invalid/other",
			EffectiveAt: at, Score: 1,
		},
	}
	r := searchHTTPRouter(searchHTTPRetriever(func(
		context.Context, string, retrieval.SearchOptions,
	) ([]retrieval.RuleChunk, error) {
		return items, nil
	}), 0)

	w := searchHTTPRequest(r, "q=test&highlight=true&limit=3", nil)
	searchHTTPWantJSON(t, w, 200, `{
		"items":[
			{
				"chunk_id":18446744073709551615,
				"document_id":11,"version_id":21,"article_id":31,"category_id":41,
				"chunk_index":2,"article_order":3,
				"version":"测试版本","title":"测试标题",
				"section_path":"测试章节","article_number":"第一条",
				"content":"测试正文","source_url":"https://example.invalid/test",
				"effective_at":"2026-10-30T04:00:00.123Z",
				"expires_at":"2026-10-30T05:00:00.123Z",
				"score":2.5,
				"highlights":{"content":["&lt;文本&gt;<em>测试</em>"]}
			},
			{
				"chunk_id":2,
				"document_id":12,"version_id":22,"article_id":32,"category_id":42,
				"chunk_index":0,"article_order":1,
				"version":"另一版本","title":"另一标题",
				"section_path":"","article_number":"",
				"content":"另一正文","source_url":"https://example.invalid/other",
				"effective_at":"2026-10-30T04:00:00.123Z",
				"expires_at":null,"score":1,"highlights":{}
			}
		],
		"count":2,
		"limit":3
	}`)
}

func TestKnowledgeSearchHTTPErrorMapping(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		status  int
		code    string
		message string
	}{
		{"invalid_query", retrieval.ErrInvalidQuery, 400,
			"invalid_search_query", "invalid search query"},
		{"invalid_candidates", retrieval.ErrInvalidCandidates, 500,
			"internal_error", "internal server error"},
		{"index_missing", retrieval.ErrIndexNotReady, 503,
			"search_index_not_ready", "search index is not ready"},
		{"verification", retrieval.ErrVerificationFailed, 503,
			"search_verification_failed", "search verification is temporarily unavailable"},
		{"unavailable", elasticsearch.ErrUnavailable, 503,
			"search_unavailable", "search is temporarily unavailable"},
		{"bad_response", elasticsearch.ErrResponse, 502,
			"invalid_search_response", "search backend returned an invalid response"},
		{"rejected", elasticsearch.ErrRejected, 502,
			"search_backend_rejected", "search backend rejected the request"},
		{"deadline", context.DeadlineExceeded, 504,
			"request_timeout", "request timed out"},
		{"verification_deadline",
			errors.Join(retrieval.ErrVerificationFailed, context.DeadlineExceeded),
			504, "request_timeout", "request timed out"},
		{"es_deadline",
			errors.Join(elasticsearch.ErrUnavailable, context.DeadlineExceeded),
			504, "request_timeout", "request timed out"},
		{"internal_candidate_priority",
			errors.Join(retrieval.ErrVerificationFailed, retrieval.ErrInvalidCandidates),
			500, "internal_error", "internal server error"},
		{"unknown", errors.New("unknown failure"), 500,
			"internal_error", "internal server error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			r := searchHTTPRouter(searchHTTPRetriever(func(
				context.Context, string, retrieval.SearchOptions,
			) ([]retrieval.RuleChunk, error) {
				calls++
				// 即使依赖同时返回候选和错误，也不能输出部分候选。
				return []retrieval.RuleChunk{{Content: searchHTTPSecret}},
					fmt.Errorf("%s: %w", searchHTTPSecret, tc.err)
			}), 0)
			w := searchHTTPRequest(r, "q="+searchHTTPSecret, nil)
			searchHTTPWantError(t, w, tc.status, tc.code, tc.message)
			if calls != 1 {
				t.Fatalf("retriever calls=%d", calls)
			}
		})
	}
}

func TestKnowledgeSearchHTTPInternalContracts(t *testing.T) {
	t.Run("missing_retriever", func(t *testing.T) {
		w := searchHTTPRequest(searchHTTPRouter(nil, 0), "q=test", nil)
		searchHTTPWantError(t, w, 500, "internal_error", "internal server error")
	})

	for _, tc := range []struct {
		name  string
		items []retrieval.RuleChunk
	}{
		{"nil_result", nil},
		{"too_many_results", make([]retrieval.RuleChunk, 2)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := searchHTTPRouter(searchHTTPRetriever(func(
				context.Context, string, retrieval.SearchOptions,
			) ([]retrieval.RuleChunk, error) {
				return tc.items, nil
			}), 0)
			w := searchHTTPRequest(r, "q=test&limit=1", nil)
			searchHTTPWantError(t, w, 500, "internal_error", "internal server error")
		})
	}

	t.Run("missing_identity_before_query", func(t *testing.T) {
		calls := 0
		r := gin.New()
		h := NewKnowledgeSearchHandler(searchHTTPRetriever(func(
			context.Context, string, retrieval.SearchOptions,
		) ([]retrieval.RuleChunk, error) {
			calls++
			return []retrieval.RuleChunk{}, nil
		}))
		r.GET(searchHTTPPath, h.Search)
		w := searchHTTPRequest(r, "q=%ZZ", nil)
		searchHTTPWantError(t, w, 401, "unauthorized", "authentication required")
		if calls != 0 || w.Header().Get("WWW-Authenticate") != "Bearer" {
			t.Fatal("missing identity must stop search and return Bearer challenge")
		}
	})
}

func TestKnowledgeSearchHTTPContext(t *testing.T) {
	t.Run("context_value_and_earlier_deadline", func(t *testing.T) {
		type key struct{}
		deadline := time.Now().Add(5 * time.Second)
		ctx, cancel := context.WithDeadline(
			context.WithValue(context.Background(), key{}, "marker"), deadline,
		)
		defer cancel()
		calls := 0
		r := searchHTTPRouter(searchHTTPRetriever(func(
			got context.Context, _ string, _ retrieval.SearchOptions,
		) ([]retrieval.RuleChunk, error) {
			calls++
			actual, ok := got.Deadline()
			if !ok || !actual.Equal(deadline) || got.Value(key{}) != "marker" {
				t.Fatal("request context or earlier deadline was lost")
			}
			return []retrieval.RuleChunk{}, nil
		}), 40*time.Second)
		w := searchHTTPRequest(r, "q=test", ctx)
		searchHTTPWantJSON(t, w, 200, `{"items":[],"count":0,"limit":10}`)
		if calls != 1 {
			t.Fatal("retriever was not called exactly once")
		}
	})

	t.Run("middleware_deadline_discards_late_success", func(t *testing.T) {
		calls := 0
		r := searchHTTPRouter(searchHTTPRetriever(func(
			ctx context.Context, _ string, _ retrieval.SearchOptions,
		) ([]retrieval.RuleChunk, error) {
			calls++
			<-ctx.Done()
			return []retrieval.RuleChunk{{Content: searchHTTPSecret}}, nil
		}), 20*time.Millisecond)
		// 即使中间件回归，父上下文也让测试有界结束。
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		w := searchHTTPRequest(r, "q=test", ctx)
		searchHTTPWantError(t, w, 504, "request_timeout", "request timed out")
		if calls != 1 {
			t.Fatalf("retriever calls=%d", calls)
		}
	})

	for _, phase := range []string{"before", "during", "dependency_error"} {
		t.Run("cancel_"+phase, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if phase == "before" {
				cancel()
			}
			calls := 0
			h := NewKnowledgeSearchHandler(searchHTTPRetriever(func(
				context.Context, string, retrieval.SearchOptions,
			) ([]retrieval.RuleChunk, error) {
				calls++
				if phase == "during" {
					cancel()
					return []retrieval.RuleChunk{{Content: searchHTTPSecret}}, nil
				}
				return nil, fmt.Errorf("wrapped: %w", context.Canceled)
			}))

			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Set("auth.user_id", uint64(99))
			c.Request = httptest.NewRequest(
				http.MethodGet, searchHTTPPath+"?q=test", nil,
			).WithContext(ctx)
			h.Search(c)

			// 直接检查 Gin Writer，避免把 Recorder 的默认 200 当作业务成功。
			if c.Writer.Written() || w.Body.Len() != 0 {
				t.Fatal("canceled handler must not write a response")
			}
			wantCalls := 1
			if phase == "before" {
				wantCalls = 0
			}
			if calls != wantCalls {
				t.Fatalf("retriever calls=%d, want=%d", calls, wantCalls)
			}
		})
	}
}
