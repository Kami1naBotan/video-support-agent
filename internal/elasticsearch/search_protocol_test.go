package elasticsearch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"video-support-agent/internal/retrieval"
)

func searchFixtureHit(id string, score float64) map[string]any {
	return map[string]any{
		"_index": protocolOld,
		"_id":    id,
		"_score": score,
		"_source": map[string]any{
			"chunk_id":             id,
			"document_id":          "11",
			"version_id":           "21",
			"article_id":           "31",
			"category_id":          "41",
			"publication_revision": "7",
			"chunk_index":          0,
			"article_order":        1,
			"version":              "测试版本",
			"title":                "测试标题",
			"section_path":         "测试章节",
			"article_number":       "第一条",
			"content":              "测试正文",
			"source_url":           "https://example.invalid/test",
			"status":               "published",
			"is_current":           true,
			"effective_at":         "2026-10-30T04:00:00.123Z",
			"expires_at":           nil,
			"withdrawn_at":         nil,
		},
	}
}

func searchFixtureEnvelope(hits ...any) map[string]any {
	if hits == nil {
		hits = []any{}
	}
	return map[string]any{
		"timed_out": false,
		"_shards": map[string]any{
			"total": 1, "successful": 1, "failed": 0,
		},
		"hits": map[string]any{"hits": hits},
	}
}

func searchDecode(t *testing.T, envelope any,
	opts retrieval.SearchOptions) ([]retrieval.Candidate, error) {
	t.Helper()
	return decodeSearchCandidates(
		context.Background(),
		[]byte(protocolJSON(t, envelope)),
		protocolOld, opts, searchTestTime(),
	)
}

func searchRequireFailure(t *testing.T, result []retrieval.Candidate,
	err error, targets ...error) {
	t.Helper()
	requireProtocolError(t, err, targets...)
	if result != nil {
		t.Fatal("failed search returned partial candidates")
	}
}

func searchAliasStep() protocolStep {
	return protocolStep{
		method: http.MethodGet,
		path:   "/_alias/" + protocolAlias,
		body:   protocolAliasBody(protocolOld),
	}
}

func searchResponseStep(body string) protocolStep {
	return protocolStep{
		method: http.MethodPost,
		path:   "/" + protocolOld + "/_search?allow_partial_search_results=false",
		body:   body,
	}
}

func TestSearchResponseSuccess(t *testing.T) {
	t.Run("all_fields_and_boundaries", func(t *testing.T) {
		hit := searchFixtureHit("10", 2)
		source := hit["_source"].(map[string]any)
		source["expires_at"] = "2026-10-30T04:00:00.124Z"
		hit["highlight"] = map[string][]string{
			"title":   {"<em>测试</em>标题"},
			"content": {"&lt;script&gt;<em>测试</em>&amp;正文"},
		}
		opts := retrieval.SearchOptions{
			DocumentID: 11, CategoryID: 41, ArticleNumber: "第一条",
			Limit: 2, Highlight: true,
		}
		got, err := searchDecode(t, searchFixtureEnvelope(hit), opts)
		expiry := searchTestTime().Add(time.Millisecond)
		want := []retrieval.Candidate{{
			PublicationRevision: 7,
			Chunk: retrieval.RuleChunk{
				ChunkID: 10, DocumentID: 11, VersionID: 21,
				ArticleID: 31, CategoryID: 41,
				ChunkIndex: 0, ArticleOrder: 1,
				Version: "测试版本", Title: "测试标题",
				SectionPath: "测试章节", ArticleNumber: "第一条",
				Content: "测试正文", SourceURL: "https://example.invalid/test",
				EffectiveAt: searchTestTime(), ExpiresAt: &expiry, Score: 2,
				Highlights: map[string][]string{
					"title":   {"<em>测试</em>标题"},
					"content": {"&lt;script&gt;<em>测试</em>&amp;正文"},
				},
			},
		}}
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("got=%+v, err=%v, want=%+v", got, err, want)
		}
	})

	t.Run("empty", func(t *testing.T) {
		got, err := searchDecode(t, searchFixtureEnvelope(),
			retrieval.SearchOptions{Limit: 1})
		if err != nil || got == nil || len(got) != 0 {
			t.Fatalf("expected non-nil empty result: %+v, %v", got, err)
		}
	})

	t.Run("lexical_tie_and_optional_values", func(t *testing.T) {
		first := searchFixtureHit("10", 0)
		second := searchFixtureHit("2", 0)
		source := first["_source"].(map[string]any)
		source["publication_revision"] = "0"
		source["section_path"] = ""
		source["article_number"] = ""
		got, err := searchDecode(t, searchFixtureEnvelope(first, second),
			retrieval.SearchOptions{Limit: 1})
		if err != nil || len(got) != 2 ||
			got[0].Chunk.ChunkID != 10 || got[1].Chunk.ChunkID != 2 {
			t.Fatalf("lexical order was lost: %+v, %v", got, err)
		}
		for _, item := range got {
			if item.Chunk.ExpiresAt != nil || item.Chunk.Highlights == nil ||
				len(item.Chunk.Highlights) != 0 {
				t.Fatal("incorrect nullable time or empty highlights")
			}
		}
		if got[0].PublicationRevision != 0 {
			t.Fatal("zero publication revision is valid")
		}
	})
}

func TestSearchResponseEnvelopeFailures(t *testing.T) {
	cases := []struct {
		name   string
		change func(map[string]any)
		want   error
	}{
		{"missing_timeout", func(e map[string]any) { delete(e, "timed_out") }, ErrResponse},
		{"null_timeout", func(e map[string]any) { e["timed_out"] = nil }, ErrResponse},
		{"timed_out", func(e map[string]any) { e["timed_out"] = true }, ErrUnavailable},
		{"terminated", func(e map[string]any) { e["terminated_early"] = true }, ErrResponse},
		{"missing_shards", func(e map[string]any) { delete(e, "_shards") }, ErrResponse},
		{"missing_hits", func(e map[string]any) { delete(e, "hits") }, ErrResponse},
		{"null_hit_array", func(e map[string]any) {
			e["hits"] = map[string]any{"hits": nil}
		}, ErrResponse},
		{"zero_shards", func(e map[string]any) {
			e["_shards"] = map[string]any{"total": 0, "successful": 0, "failed": 0}
		}, ErrResponse},
		{"partial_shards", func(e map[string]any) {
			e["_shards"] = map[string]any{"total": 2, "successful": 1, "failed": 0}
		}, ErrResponse},
		{"failed_shards", func(e map[string]any) {
			e["_shards"] = map[string]any{"total": 1, "successful": 1, "failed": 1}
		}, ErrResponse},
		{"missing_shard_field", func(e map[string]any) {
			e["_shards"] = map[string]any{"total": 1, "successful": 1}
		}, ErrResponse},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			envelope := searchFixtureEnvelope(searchFixtureHit("10", 2))
			tc.change(envelope)
			got, err := searchDecode(t, envelope, retrieval.SearchOptions{Limit: 1})
			searchRequireFailure(t, got, err, tc.want)
			if tc.name == "timed_out" {
				requireProtocolError(t, err, context.DeadlineExceeded)
			}
		})
	}

	t.Run("over_candidate_budget", func(t *testing.T) {
		hits := make([]any, 6)
		for i := range hits {
			hits[i] = searchFixtureHit(fmt.Sprint(i+1), float64(6-i))
		}
		got, err := searchDecode(t, searchFixtureEnvelope(hits...),
			retrieval.SearchOptions{Limit: 1})
		searchRequireFailure(t, got, err, ErrResponse)
	})

	for name, body := range map[string][]byte{
		"malformed":     []byte("{"),
		"trailing_json": []byte(`{} {}`),
		"invalid_utf8":  {0xff},
		"oversized":     []byte(strings.Repeat(" ", MaxResponseBytes+1)),
		"score_overflow": []byte(strings.Replace(
			protocolJSON(t, searchFixtureEnvelope(searchFixtureHit("10", 2))),
			`"_score":2`, `"_score":1e999`, 1)),
	} {
		t.Run(name, func(t *testing.T) {
			got, err := decodeSearchCandidates(context.Background(),
				body, protocolOld, retrieval.SearchOptions{Limit: 1}, searchTestTime())
			searchRequireFailure(t, got, err, ErrResponse)
		})
	}
}

func TestSearchResponseRequiredSourceFields(t *testing.T) {
	fields := []string{
		"chunk_id", "document_id", "version_id", "article_id", "category_id",
		"publication_revision", "chunk_index", "article_order", "version",
		"title", "section_path", "article_number", "content", "source_url",
		"status", "is_current", "effective_at", "expires_at", "withdrawn_at",
	}
	for _, field := range fields {
		t.Run("missing_"+field, func(t *testing.T) {
			hit := searchFixtureHit("10", 2)
			delete(hit["_source"].(map[string]any), field)
			got, err := searchDecode(t, searchFixtureEnvelope(hit),
				retrieval.SearchOptions{Limit: 1})
			searchRequireFailure(t, got, err, ErrResponse)
		})
		if field == "expires_at" || field == "withdrawn_at" {
			continue
		}
		t.Run("null_"+field, func(t *testing.T) {
			hit := searchFixtureHit("10", 2)
			hit["_source"].(map[string]any)[field] = nil
			got, err := searchDecode(t, searchFixtureEnvelope(hit),
				retrieval.SearchOptions{Limit: 1})
			searchRequireFailure(t, got, err, ErrResponse)
		})
	}
}

func TestSearchResponseInvalidHits(t *testing.T) {
	cases := []struct {
		name   string
		change func(map[string]any)
	}{
		{"wrong_index", func(h map[string]any) { h["_index"] = protocolIndex }},
		{"missing_score", func(h map[string]any) { delete(h, "_score") }},
		{"null_score", func(h map[string]any) { h["_score"] = nil }},
		{"negative_score", func(h map[string]any) { h["_score"] = -1 }},
		{"id_mismatch", func(h map[string]any) { h["_id"] = "999" }},
		{"missing_source", func(h map[string]any) { delete(h, "_source") }},
		{"null_source", func(h map[string]any) { h["_source"] = nil }},
		{"array_source", func(h map[string]any) { h["_source"] = []any{} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bad := searchFixtureHit("2", 1)
			tc.change(bad)
			// 第一项合法，第二项损坏；必须整体失败。
			got, err := searchDecode(t,
				searchFixtureEnvelope(searchFixtureHit("10", 2), bad),
				retrieval.SearchOptions{Limit: 1})
			searchRequireFailure(t, got, err, ErrResponse)
		})
	}

	for _, tc := range []struct {
		name  string
		field string
		value any
	}{
		{"zero_id", "document_id", "0"},
		{"leading_zero", "document_id", "011"},
		{"signed_id", "document_id", "+11"},
		{"numeric_id", "document_id", 11},
		{"overflow_id", "document_id", "18446744073709551616"},
		{"bad_revision", "publication_revision", "-1"},
		{"negative_chunk_index", "chunk_index", -1},
		{"overflow_chunk_index", "chunk_index", uint64(1) << 32},
		{"zero_article_order", "article_order", 0},
		{"blank_version", "version", " "},
		{"blank_title", "title", " "},
		{"blank_content", "content", " "},
		{"blank_source_url", "source_url", " "},
		{"not_current", "is_current", false},
		{"draft", "status", "draft"},
		{"withdrawn", "withdrawn_at", "2026-10-30T04:00:00.123Z"},
		{"future", "effective_at", "2026-10-30T04:00:00.124Z"},
		{"invalid_time", "effective_at", "not-a-time"},
		{"zero_time", "effective_at", "0001-01-01T00:00:00Z"},
		{"expiry_equal_now", "expires_at", "2026-10-30T04:00:00.123Z"},
		{"expiry_before_now", "expires_at", "2026-10-30T04:00:00.122Z"},
		{"wrong_document", "document_id", "12"},
		{"wrong_category", "category_id", "42"},
		{"wrong_article", "article_number", "第二条"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hit := searchFixtureHit("10", 2)
			hit["_source"].(map[string]any)[tc.field] = tc.value
			got, err := searchDecode(t, searchFixtureEnvelope(hit),
				retrieval.SearchOptions{
					DocumentID: 11, CategoryID: 41, ArticleNumber: "第一条", Limit: 1,
				})
			searchRequireFailure(t, got, err, ErrResponse)
		})
	}

	for _, tc := range []struct {
		name   string
		first  map[string]any
		second map[string]any
	}{
		{"duplicate", searchFixtureHit("10", 2), searchFixtureHit("10", 1)},
		{"score_ascending", searchFixtureHit("10", 1), searchFixtureHit("2", 2)},
		{"tie_wrong_lexical_order", searchFixtureHit("2", 1), searchFixtureHit("10", 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := searchDecode(t, searchFixtureEnvelope(tc.first, tc.second),
				retrieval.SearchOptions{Limit: 1})
			searchRequireFailure(t, got, err, ErrResponse)
		})
	}
}

func TestSearchResponseHighlights(t *testing.T) {
	for _, tc := range []struct {
		name       string
		enabled    bool
		highlights map[string][]string
	}{
		{"disabled", false, map[string][]string{"title": {"测试"}}},
		{"unknown_field", true, map[string][]string{"source_url": {"测试"}}},
		{"empty_list", true, map[string][]string{"content": {}}},
		{"empty_fragment", true, map[string][]string{"content": {""}}},
		{"too_many_titles", true, map[string][]string{"title": {"甲", "乙"}}},
		{"too_many_sections", true, map[string][]string{"section_path": {"甲", "乙", "丙"}}},
		{"too_many_contents", true, map[string][]string{"content": {"甲", "乙", "丙", "丁"}}},
		{"script", true, map[string][]string{"content": {"<script>测试</script>"}}},
		{"tag_attributes", true, map[string][]string{"content": {`<em onclick="x">测试</em>`}}},
		{"raw_angle", true, map[string][]string{"content": {"甲 > 乙"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hit := searchFixtureHit("10", 2)
			hit["highlight"] = tc.highlights
			got, err := searchDecode(t, searchFixtureEnvelope(hit),
				retrieval.SearchOptions{Limit: 1, Highlight: tc.enabled})
			searchRequireFailure(t, got, err, ErrResponse)
		})
	}

	t.Run("fragment_limits_and_copy", func(t *testing.T) {
		input := map[string][]string{
			"title":        {"<em>标题</em>"},
			"section_path": {"章节一", "章节二"},
			"content":      {"正文一", "正文二", "正文三"},
		}
		got, err := decodeSearchHighlights(input, true)
		if err != nil || !reflect.DeepEqual(got, input) {
			t.Fatalf("valid highlights rejected: %v", err)
		}
		got["content"][0] = "changed"
		got["title"] = []string{"changed"}
		if input["content"][0] != "正文一" || input["title"][0] != "<em>标题</em>" {
			t.Fatal("highlight output aliases input memory")
		}
	})

	t.Run("invalid_utf8_fragment", func(t *testing.T) {
		_, err := decodeSearchHighlights(
			map[string][]string{"content": {string([]byte{0xff})}}, true)
		requireProtocolError(t, err, ErrResponse)
	})
}

func TestSearchProtocolRealHTTP(t *testing.T) {
	opts := retrieval.SearchOptions{Limit: 2}
	wantRequest, err := BuildSearchQuery("测试", opts, searchTestTime())
	if err != nil {
		t.Fatal(err)
	}
	responseBody := protocolJSON(t, searchFixtureEnvelope(searchFixtureHit("10", 2)))
	var calls atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Accept") != "application/json" {
			t.Error("missing Accept header")
		}
		switch n {
		case 1:
			if r.Method != http.MethodGet || r.URL.RequestURI() != "/_alias/"+protocolAlias {
				t.Errorf("unexpected alias request: %s %s", r.Method, r.URL.RequestURI())
			}
			_, _ = io.WriteString(w, protocolAliasBody(protocolOld))
		case 2:
			wantPath := "/" + protocolOld + "/_search?allow_partial_search_results=false"
			if r.Method != http.MethodPost || r.URL.RequestURI() != wantPath {
				t.Errorf("search did not pin physical target: %s %s",
					r.Method, r.URL.RequestURI())
			}
			if r.Header.Get("Content-Type") != "application/json" {
				t.Error("missing JSON content type")
			}
			body, readErr := io.ReadAll(r.Body)
			if readErr != nil || string(body) != string(wantRequest) {
				t.Errorf("incorrect search request body: %s, %v", body, readErr)
			}
			_, _ = io.WriteString(w, responseBody)
		default:
			t.Error("unexpected retry or alias reread")
			http.Error(w, "unexpected request", http.StatusInternalServerError)
		}
	}))
	t.Cleanup(server.Close)
	client := protocolClient(t, server.URL)

	if calls.Load() != 0 {
		t.Fatal("constructor performed network I/O")
	}
	got, err := client.SearchCandidates(context.Background(), " 测试 ", opts, searchTestTime())
	if err != nil || len(got) != 1 || got[0].Chunk.ChunkID != 10 || calls.Load() != 2 {
		t.Fatalf("unexpected search result: %+v, err=%v, calls=%d", got, err, calls.Load())
	}
}

func TestSearchProtocolFailuresAndGuards(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   error
	}{
		{404, retrieval.ErrIndexNotReady},
		{408, ErrUnavailable},
		{429, ErrUnavailable},
		{500, ErrUnavailable},
		{503, ErrUnavailable},
		{400, ErrRejected},
		{401, ErrRejected},
		{403, ErrRejected},
		{302, ErrRejected},
	} {
		t.Run(fmt.Sprintf("search_http_%d", tc.status), func(t *testing.T) {
			step := searchResponseStep(protocolSecret)
			step.status = tc.status
			client := protocolScript(t, searchAliasStep(), step)
			got, err := client.SearchCandidates(context.Background(), "测试",
				retrieval.SearchOptions{Limit: 1}, searchTestTime())
			searchRequireFailure(t, got, err, tc.want)
		})
	}

	for _, tc := range []struct {
		name string
		step protocolStep
		want error
	}{
		{
			"alias_missing",
			protocolStep{
				method: http.MethodGet, path: "/_alias/" + protocolAlias,
				status: 404, body: `{"error":"missing","status":404}`,
			},
			retrieval.ErrIndexNotReady,
		},
		{
			"alias_ambiguous",
			protocolStep{
				method: http.MethodGet, path: "/_alias/" + protocolAlias,
				body: `{"first":{"aliases":{}},"second":{"aliases":{}}}`,
			},
			ErrResponse,
		},
		{
			"alias_transport",
			protocolStep{
				method: http.MethodGet, path: "/_alias/" + protocolAlias,
				err: errors.New(protocolSecret),
			},
			ErrUnavailable,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := protocolScript(t, tc.step)
			got, err := client.SearchCandidates(context.Background(), "测试",
				retrieval.SearchOptions{Limit: 1}, searchTestTime())
			searchRequireFailure(t, got, err, tc.want)
		})
	}

	t.Run("search_transport", func(t *testing.T) {
		step := searchResponseStep("")
		step.err = errors.New(protocolSecret)
		client := protocolScript(t, searchAliasStep(), step)
		got, err := client.SearchCandidates(context.Background(), "测试",
			retrieval.SearchOptions{Limit: 1}, searchTestTime())
		searchRequireFailure(t, got, err, ErrUnavailable)
	})

	t.Run("invalid_input_before_network", func(t *testing.T) {
		client := protocolScript(t)
		got, err := client.SearchCandidates(context.Background(), "",
			retrieval.SearchOptions{}, searchTestTime())
		searchRequireFailure(t, got, err, retrieval.ErrInvalidQuery)
		got, err = client.SearchCandidates(context.Background(), "测试",
			retrieval.SearchOptions{}, time.Time{})
		searchRequireFailure(t, got, err, retrieval.ErrInvalidQuery)
	})

	t.Run("canceled_before_network", func(t *testing.T) {
		client := protocolScript(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		got, err := client.SearchCandidates(ctx, "测试",
			retrieval.SearchOptions{Limit: 1}, searchTestTime())
		searchRequireFailure(t, got, err, context.Canceled)
	})
}

func TestSearchProtocolBodyLifecycle(t *testing.T) {
	valid := protocolJSON(t, searchFixtureEnvelope(searchFixtureHit("10", 2)))
	for _, tc := range []struct {
		name     string
		reader   io.Reader
		want     error
		cancelAt string
	}{
		{"success", strings.NewReader(valid), nil, ""},
		{"malformed", strings.NewReader("{"), ErrResponse, ""},
		{"oversized", strings.NewReader(strings.Repeat("x", MaxResponseBytes+1)), ErrResponse, ""},
		{"read_failure", protocolBrokenReader{}, ErrUnavailable, ""},
		{"cancel_alias", strings.NewReader(valid), context.Canceled, "alias"},
		{"cancel_search", strings.NewReader(valid), context.Canceled, "search"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			client := protocolClient(t, "http://127.0.0.1:1")
			aliasBody := &protocolBody{Reader: strings.NewReader(protocolAliasBody(protocolOld))}
			searchBody := &protocolBody{Reader: tc.reader}
			calls := 0
			client.httpClient.Transport = protocolTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				var body io.ReadCloser
				switch calls {
				case 1:
					body = aliasBody
					if tc.cancelAt == "alias" {
						cancel()
					}
				case 2:
					body = searchBody
					if tc.cancelAt == "search" {
						cancel()
					}
				default:
					t.Fatal("unexpected retry")
				}
				return &http.Response{
					StatusCode: 200, Header: make(http.Header), Body: body, Request: r,
				}, nil
			})

			got, err := client.SearchCandidates(ctx, "测试",
				retrieval.SearchOptions{Limit: 1}, searchTestTime())
			if tc.want != nil {
				searchRequireFailure(t, got, err, tc.want)
			} else if err != nil || len(got) != 1 {
				t.Fatalf("unexpected success result: %+v, %v", got, err)
			}
			wantCalls := 2
			if tc.cancelAt == "alias" {
				wantCalls = 1
			}
			if calls != wantCalls || !aliasBody.closed ||
				(calls == 2 && !searchBody.closed) {
				t.Fatalf("resource lifecycle incorrect: calls=%d, alias closed=%v, search closed=%v",
					calls, aliasBody.closed, searchBody.closed)
			}
		})
	}
}
