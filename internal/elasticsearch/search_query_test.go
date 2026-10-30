package elasticsearch

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"testing"
	"time"

	"video-support-agent/internal/retrieval"
)

func searchTestTime() time.Time {
	return time.Date(2026, 10, 30, 4, 0, 0, 123000000, time.UTC)
}

func searchJSONValue(t *testing.T, data []byte) any {
	t.Helper()
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatalf("invalid test JSON: %v", err)
	}
	return value
}

func searchAssertJSON(t *testing.T, got any, expected string) {
	t.Helper()
	actual := searchJSONValue(t, []byte(protocolJSON(t, got)))
	want := searchJSONValue(t, []byte(expected))
	if !reflect.DeepEqual(actual, want) {
		t.Fatalf("JSON mismatch\ngot:  %s\nwant: %s",
			protocolJSON(t, actual), expected)
	}
}

func searchObject(t *testing.T, value any) map[string]any {
	t.Helper()
	object, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("expected object, got %T", value)
	}
	return object
}

func searchArray(t *testing.T, value any) []any {
	t.Helper()
	array, ok := value.([]any)
	if !ok {
		t.Fatalf("expected array, got %T", value)
	}
	return array
}

func TestSearchQueryCombinedContract(t *testing.T) {
	at := time.Date(
		2026, 10, 30, 12, 0, 0, 123999999,
		time.FixedZone("test-zone", 8*60*60),
	)
	data, err := BuildSearchQuery(" 测试正文 ", retrieval.SearchOptions{
		DocumentID:    11,
		CategoryID:    41,
		ArticleNumber: " 第一条 ",
		Section:       " 测试章节 ",
		Limit:         2,
		Highlight:     true,
	}, at)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > MaxRequestBytes {
		t.Fatal("request exceeds transport budget")
	}
	root := searchObject(t, searchJSONValue(t, data))
	if len(root) != 6 || root["size"] != float64(10) ||
		root["track_total_hits"] != false {
		t.Fatalf("unexpected root contract: %s", data)
	}

	searchAssertJSON(t, root["sort"],
		`[{"_score":"desc"},{"chunk_id":"asc"}]`)
	searchAssertJSON(t, root["query"], `{
		"bool":{
			"must":[{
				"multi_match":{
					"query":"测试正文",
					"fields":["title^3","section_path^2","content"],
					"type":"best_fields",
					"operator":"or"
				}
			}],
			"filter":[
				{"term":{"is_current":true}},
				{"term":{"status":"published"}},
				{"range":{"effective_at":{"lte":"2026-10-30T04:00:00.123Z"}}},
				{"bool":{
					"should":[
						{"bool":{"must_not":[{"exists":{"field":"expires_at"}}]}},
						{"range":{"expires_at":{"gt":"2026-10-30T04:00:00.123Z"}}}
					],
					"minimum_should_match":1
				}},
				{"term":{"document_id":"11"}},
				{"term":{"category_id":"41"}},
				{"term":{"article_number":"第一条"}},
				{"match":{"section_path":{"query":"测试章节","operator":"or"}}}
			],
			"must_not":[{"exists":{"field":"withdrawn_at"}}]
		}
	}`)
	searchAssertJSON(t, root["highlight"], `{
		"type":"unified",
		"encoder":"html",
		"pre_tags":["<em>"],
		"post_tags":["</em>"],
		"require_field_match":true,
		"highlight_query":{
			"bool":{
				"should":[
					{"multi_match":{
						"query":"测试正文",
						"fields":["title^3","section_path^2","content"],
						"type":"best_fields","operator":"or"
					}},
					{"match":{"section_path":{"query":"测试章节","operator":"or"}}}
				],
				"minimum_should_match":1
			}
		},
		"fields":{
			"title":{"number_of_fragments":0,"no_match_size":0},
			"section_path":{
				"number_of_fragments":2,"fragment_size":120,"no_match_size":0
			},
			"content":{
				"number_of_fragments":3,"fragment_size":160,"no_match_size":0
			}
		}
	}`)

	fields := searchArray(t, root["_source"])
	gotFields := make([]string, len(fields))
	for i, value := range fields {
		field, ok := value.(string)
		if !ok {
			t.Fatal("source projection contains a non-string field")
		}
		gotFields[i] = field
	}
	wantFields := []string{
		"chunk_id", "document_id", "version_id", "article_id",
		"category_id", "publication_revision", "chunk_index", "article_order",
		"version", "title", "section_path", "article_number", "content",
		"source_url", "status", "is_current",
		"effective_at", "expires_at", "withdrawn_at",
	}
	sort.Strings(gotFields)
	sort.Strings(wantFields)
	if !reflect.DeepEqual(gotFields, wantFields) {
		t.Fatalf("source projection differs: %v", gotFields)
	}
}

func TestSearchQueryOptionalCombinations(t *testing.T) {
	// 四个可选过滤条件的全部组合；每个条件都必须位于 filter，
	// 不能变成可选 should 条件。
	for mask := 0; mask < 16; mask++ {
		t.Run(fmt.Sprintf("filters_%02d", mask), func(t *testing.T) {
			opts := retrieval.SearchOptions{}
			want := make([]any, 0, 4)
			if mask&1 != 0 {
				opts.DocumentID = 11
				want = append(want, map[string]any{
					"term": map[string]any{"document_id": "11"},
				})
			}
			if mask&2 != 0 {
				opts.CategoryID = 41
				want = append(want, map[string]any{
					"term": map[string]any{"category_id": "41"},
				})
			}
			if mask&4 != 0 {
				opts.ArticleNumber = "第一条"
				want = append(want, map[string]any{
					"term": map[string]any{"article_number": "第一条"},
				})
			}
			if mask&8 != 0 {
				opts.Section = "章节"
				want = append(want, map[string]any{
					"match": map[string]any{
						"section_path": map[string]any{
							"query": "章节", "operator": "or",
						},
					},
				})
			}

			data, err := BuildSearchQuery("正文", opts, searchTestTime())
			if err != nil {
				t.Fatal(err)
			}
			root := searchObject(t, searchJSONValue(t, data))
			if root["size"] != float64(50) {
				t.Fatal("default limit must request 50 candidates")
			}
			if _, exists := root["highlight"]; exists {
				t.Fatal("highlight must be absent when disabled")
			}
			query := searchObject(t, root["query"])
			boolean := searchObject(t, query["bool"])
			filters := searchArray(t, boolean["filter"])
			if len(filters) != 4+len(want) {
				t.Fatalf("unexpected filter count: %d", len(filters))
			}
			searchAssertJSON(t, filters[4:], protocolJSON(t, want))
			if len(searchArray(t, boolean["must"])) != 1 {
				t.Fatal("text query must remain mandatory")
			}
		})
	}

	for _, tc := range []struct {
		name          string
		opts          retrieval.SearchOptions
		wantHighlight string
	}{
		{
			"section_only",
			retrieval.SearchOptions{Section: "章节", Highlight: true},
			`{"bool":{"should":[
				{"match":{"section_path":{"query":"章节","operator":"or"}}}
			],"minimum_should_match":1}}`,
		},
		{
			"article_only",
			retrieval.SearchOptions{ArticleNumber: "第一条", Highlight: true},
			`{"match_none":{}}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, err := BuildSearchQuery("", tc.opts, searchTestTime())
			if err != nil {
				t.Fatal(err)
			}
			root := searchObject(t, searchJSONValue(t, data))
			boolean := searchObject(t, searchObject(t, root["query"])["bool"])
			if _, exists := boolean["must"]; exists {
				t.Fatal("filter-only query must not invent a text condition")
			}
			highlight := searchObject(t, root["highlight"])
			searchAssertJSON(t, highlight["highlight_query"], tc.wantHighlight)
		})
	}
}

func TestSearchQueryBoundsAndLiteralInput(t *testing.T) {
	for _, tc := range []struct {
		limit int
		size  int
	}{
		{0, 50}, {1, 5}, {50, 250},
	} {
		t.Run(fmt.Sprintf("limit_%d", tc.limit), func(t *testing.T) {
			const literal = `标题:"测试" OR * <script>`
			data, err := BuildSearchQuery(literal,
				retrieval.SearchOptions{Limit: tc.limit}, searchTestTime())
			if err != nil {
				t.Fatal(err)
			}
			root := searchObject(t, searchJSONValue(t, data))
			if root["size"] != float64(tc.size) {
				t.Fatalf("incorrect candidate size: %v", root["size"])
			}
			boolean := searchObject(t, searchObject(t, root["query"])["bool"])
			must := searchArray(t, boolean["must"])
			clause := searchObject(t, must[0])
			text := searchObject(t, clause["multi_match"])
			if len(clause) != 1 || text["query"] != literal {
				t.Fatal("user input must remain literal multi_match text")
			}
		})
	}

	for _, tc := range []struct {
		name  string
		query string
		opts  retrieval.SearchOptions
		at    time.Time
	}{
		{"empty", "", retrieval.SearchOptions{}, searchTestTime()},
		{"limit", "测试", retrieval.SearchOptions{Limit: 51}, searchTestTime()},
		{"utf8", string([]byte{0xff}), retrieval.SearchOptions{}, searchTestTime()},
		{"zero_time", "测试", retrieval.SearchOptions{}, time.Time{}},
		{"early_year", "测试", retrieval.SearchOptions{},
			time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC)},
		{"late_year", "测试", retrieval.SearchOptions{},
			time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, err := BuildSearchQuery(tc.query, tc.opts, tc.at)
			requireProtocolError(t, err, retrieval.ErrInvalidQuery)
			if data != nil {
				t.Fatal("invalid query returned request bytes")
			}
		})
	}
}
