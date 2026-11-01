package milvus

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"video-support-agent/internal/embedding"
	"video-support-agent/internal/retrieval"
)

var vsOutputFields = []string{
	"chunk_id", "document_id", "version_id", "article_id",
	"category", "publication_revision", "version",
	"effective_at", "has_expires_at", "expires_at",
}

type vsFixture struct {
	base       *mvFixture
	query      *PreparedQuery
	rows       any
	wantVector []float32
	edit       func(string, any) any
	before     func(*http.Request, string) error
}

func vsNew(t *testing.T) *vsFixture {
	t.Helper()
	base := mvNewFixture(t, 2)
	base.alias = mvTarget
	batch := vqBatch()
	q := &PreparedQuery{
		Options: retrieval.SearchOptions{
			DocumentID: 10, CategoryID: 30, Limit: 10,
		},
		At:             vqTime().UTC().Truncate(time.Millisecond),
		CandidateLimit: 50,
		Identity:       batch.Identity,
		Vector:         batch.Vectors[0],
	}

	rows := make([]map[string]any, 0, 2)
	for i, record := range base.prepared.Records {
		var row map[string]any
		mvDecode(t, mvJSON(t, record), &row)
		delete(row, "embedding")
		row["distance"] = 0.9 - float64(i)*0.1
		rows = append(rows, row)
	}

	f := &vsFixture{
		base: base, query: q, rows: rows,
		wantVector: append([]float32(nil), q.Vector...),
	}
	base.client.http.Transport = mvTransport(f.roundTrip)
	return f
}

func (f *vsFixture) roundTrip(r *http.Request) (*http.Response, error) {
	t := f.base.t
	path := strings.TrimPrefix(r.URL.Path, "/v2/vectordb/")
	switch path {
	case "aliases/list", "aliases/describe", "collections/describe",
		"indexes/describe", "collections/get_load_state", "entities/search":
	default:
		t.Fatalf("unexpected or mutating endpoint: %s", path)
	}
	if r.Method != http.MethodPost || r.URL.RawQuery != "" ||
		r.Header.Get("Content-Type") != "application/json" ||
		r.Header.Get("Accept") != "application/json" {
		t.Fatal("invalid HTTP request contract")
	}
	if _, ok := r.Context().Deadline(); !ok {
		t.Fatal("request has no deadline")
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	if len(raw) > 2<<20 {
		t.Fatal("request exceeded byte budget")
	}
	var req map[string]json.RawMessage
	mvDecode(t, raw, &req)
	mvMatch(t, req["dbName"], "default")
	if value, exists := req["collectionName"]; exists {
		mvMatch(t, value, mvTarget)
	}
	if value, exists := req["aliasName"]; exists {
		mvMatch(t, value, mvAlias)
	}

	f.base.trace = append(f.base.trace, path)
	f.base.calls[path]++
	if f.before != nil {
		if err := f.before(r, path); err != nil {
			return nil, err
		}
	}

	var data any
	if path == "entities/search" {
		mvMatch(t, req["annsField"], "embedding")
		mvMatch(t, req["data"], [][]float32{f.wantVector})
		mvMatch(t, req["limit"], f.query.CandidateLimit)
		mvMatch(t, req["outputFields"], vsOutputFields)
		mvMatch(t, req["consistencyLevel"], "Strong")

		ef := 64
		if f.query.CandidateLimit > ef {
			ef = f.query.CandidateLimit
		}
		mvMatch(t, req["searchParams"], map[string]any{
			"metricType": "COSINE",
			"params":     map[string]any{"ef": ef},
		})

		at := strconv.FormatInt(f.query.At.UnixMilli(), 10)
		filter := "effective_at <= " + at +
			" and (has_expires_at == false or expires_at > " + at + ")"
		if f.query.Options.DocumentID != 0 {
			filter += ` and document_id == "` +
				strconv.FormatUint(f.query.Options.DocumentID, 10) + `"`
		}
		if f.query.Options.CategoryID != 0 {
			filter += ` and category == "` +
				strconv.FormatUint(f.query.Options.CategoryID, 10) + `"`
		}
		mvMatch(t, req["filter"], filter)
		data = f.rows
	} else {
		data = f.base.reply(path, req)
	}
	if f.edit != nil {
		data = f.edit(path, data)
	}
	body := mvJSON(t, map[string]any{"code": 0, "data": data})
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(string(body))),
	}, nil
}

func vsFail(t *testing.T, hits []SearchHit, err, want error) {
	t.Helper()
	if hits != nil || !errors.Is(err, want) {
		t.Fatalf("hits=%v err=%v; want nil and %v", hits, err, want)
	}
}

func TestVectorSearchProtocol(t *testing.T) {
	for _, mode := range []string{
		"both_filters", "document_only", "category_only",
		"no_filters", "maximum", "alias_switch", "vector_copy", "expiration",
	} {
		t.Run(mode, func(t *testing.T) {
			f := vsNew(t)
			switch mode {
			case "document_only":
				f.query.Options.CategoryID = 0
			case "category_only":
				f.query.Options.DocumentID = 0
			case "no_filters":
				f.query.Options.DocumentID = 0
				f.query.Options.CategoryID = 0
			case "maximum":
				f.query.Options.Limit = 50
				f.query.CandidateLimit = 250
			case "alias_switch":
				f.edit = func(path string, data any) any {
					if path == "collections/describe" {
						f.base.alias = "another_target"
					}
					return data
				}
			case "vector_copy":
				f.before = func(_ *http.Request, path string) error {
					if path == "aliases/list" {
						f.query.Vector[0] = 0.25
					}
					return nil
				}
			case "expiration":
				for _, row := range f.rows.([]map[string]any) {
					row["has_expires_at"] = true
					row["expires_at"] = f.query.At.Add(time.Hour).UnixMilli()
				}
			}

			hits, err := f.base.client.SearchPrepared(mvContext(t), f.query)
			if err != nil || len(hits) != 2 {
				t.Fatalf("hits=%v err=%v", hits, err)
			}
			for i, hit := range hits {
				var expires *time.Time
				if mode == "expiration" {
					value := f.query.At.Add(time.Hour)
					expires = &value
				}
				want := SearchHit{
					ChunkID: uint64(i + 1), DocumentID: 10, VersionID: 20,
					ArticleID: uint64(100 + i), CategoryID: 30,
					PublicationRevision: 2, Version: "test-v1",
					EffectiveAt: time.Date(2026, 10, 31, 23, 0, 0, 0, time.UTC),
					ExpiresAt:   expires, Score: 0.9 - float64(i)*0.1,
				}
				if !reflect.DeepEqual(hit, want) {
					t.Fatalf("hit=%+v want=%+v", hit, want)
				}
			}
			wantTrace := []string{
				"aliases/list", "aliases/describe", "collections/describe",
				"indexes/describe", "collections/get_load_state",
				"entities/search", "collections/describe",
			}
			if !reflect.DeepEqual(f.base.trace, wantTrace) {
				t.Fatalf("unexpected request sequence: %v", f.base.trace)
			}
		})
	}
}

func TestVectorSearchPreflight(t *testing.T) {
	for _, mode := range []string{
		"nil_query", "zero_limit", "large_limit", "budget",
		"section", "article_number", "highlight", "time",
		"dimensions", "zero_vector", "model", "digest", "no_deadline",
	} {
		t.Run(mode, func(t *testing.T) {
			f := vsNew(t)
			q := f.query
			ctx := mvContext(t)
			want := error(ErrInput)
			switch mode {
			case "nil_query":
				q = nil
			case "zero_limit":
				q.Options.Limit = 0
			case "large_limit":
				q.Options.Limit = 51
			case "budget":
				q.CandidateLimit--
			case "section":
				q.Options.Section = "章节"
			case "article_number":
				q.Options.ArticleNumber = "1"
			case "highlight":
				q.Options.Highlight = true
			case "time":
				q.At = time.Time{}
			case "dimensions":
				q.Vector = q.Vector[:1023]
			case "zero_vector":
				q.Vector[0] = 0
			case "model":
				q.Identity.Model = "wrong"
				want = embedding.ErrModelMismatch
			case "digest":
				q.Identity.Digest = "invalid"
			case "no_deadline":
				ctx = context.Background()
			}
			hits, err := f.base.client.SearchPrepared(ctx, q)
			vsFail(t, hits, err, want)
			if len(f.base.trace) != 0 {
				t.Fatal("invalid prepared input reached HTTP")
			}
		})
	}
}

func TestVectorSearchReadiness(t *testing.T) {
	for _, mode := range []string{
		"alias_absent", "model_contract", "index_pending", "index_contract",
		"loading", "not_loaded", "missing_load", "unknown_load", "changed_id",
	} {
		t.Run(mode, func(t *testing.T) {
			f := vsNew(t)
			want := error(ErrResponse)
			searchCalls := 0
			if mode == "alias_absent" {
				f.base.alias = ""
				want = retrieval.ErrIndexNotReady
			}
			if mode == "index_pending" || mode == "loading" || mode == "not_loaded" {
				want = retrieval.ErrIndexNotReady
			}
			if mode == "changed_id" {
				searchCalls = 1
			}
			f.edit = func(path string, data any) any {
				switch path {
				case "collections/describe":
					d := data.(map[string]any)
					if mode == "model_contract" {
						identity := mvIdentity()
						identity.Digest = strings.Repeat("b", 64)
						d["description"] = mvDescription(t, identity)
					}
					if mode == "changed_id" && f.base.calls[path] == 2 {
						d["collectionID"] = int64(124)
					}
				case "indexes/describe":
					index := data.([]map[string]any)[0]
					if mode == "index_pending" {
						index["indexState"] = "InProgress"
					}
					if mode == "index_contract" {
						index["metricType"] = "L2"
					}
				case "collections/get_load_state":
					d := data.(map[string]any)
					switch mode {
					case "loading":
						d["loadState"] = "LoadStateLoading"
					case "not_loaded":
						d["loadState"] = "LoadStateNotLoad"
					case "missing_load":
						delete(d, "loadState")
					case "unknown_load":
						d["loadState"] = "unexpected"
					}
				}
				return data
			}
			hits, err := f.base.client.SearchPrepared(mvContext(t), f.query)
			vsFail(t, hits, err, want)
			if f.base.calls["entities/search"] != searchCalls {
				t.Fatal("search continued after a readiness failure")
			}
		})
	}
}

func TestVectorSearchResponses(t *testing.T) {
	fields := append(append([]string(nil), vsOutputFields...), "distance")
	for _, field := range fields {
		for _, action := range []string{"missing", "null"} {
			t.Run(action+"_"+field, func(t *testing.T) {
				f := vsNew(t)
				row := f.rows.([]map[string]any)[0]
				if action == "missing" {
					delete(row, field)
				} else {
					row[field] = nil
				}
				hits, err := f.base.client.SearchPrepared(mvContext(t), f.query)
				vsFail(t, hits, err, ErrResponse)
			})
		}
	}

	for _, mode := range []string{
		"empty", "null_list", "object", "too_many", "duplicate", "order",
		"extra_field", "numeric_id", "zero_id", "noncanonical_id", "overflow_id",
		"blank_version", "long_version", "future", "expired", "expiry_without_flag",
		"wrong_document", "wrong_category", "bad_score", "overflow_score",
		"score_rounding", "zero_revision",
	} {
		t.Run(mode, func(t *testing.T) {
			f := vsNew(t)
			rows := f.rows.([]map[string]any)
			row := rows[0]
			success := false
			switch mode {
			case "empty":
				f.rows = []map[string]any{}
				success = true
			case "null_list":
				f.rows = nil
			case "object":
				f.rows = map[string]any{}
			case "too_many":
				f.rows = make([]map[string]any, 51)
			case "duplicate":
				rows[1] = row
			case "order":
				rows[1]["distance"] = 0.95
			case "extra_field":
				row["content"] = "must not be returned"
			case "numeric_id":
				row["chunk_id"] = 1
			case "zero_id":
				row["chunk_id"] = "0"
			case "noncanonical_id":
				row["chunk_id"] = "01"
			case "overflow_id":
				row["chunk_id"] = "18446744073709551616"
			case "blank_version":
				row["version"] = " "
			case "long_version":
				row["version"] = strings.Repeat("x", 257)
			case "future":
				row["effective_at"] = f.query.At.Add(time.Millisecond).UnixMilli()
			case "expired":
				row["has_expires_at"] = true
				row["expires_at"] = f.query.At.UnixMilli()
			case "expiry_without_flag":
				row["expires_at"] = f.query.At.Add(time.Hour).UnixMilli()
			case "wrong_document":
				row["document_id"] = "99"
			case "wrong_category":
				row["category"] = "99"
			case "bad_score":
				row["distance"] = -1.1
			case "overflow_score":
				row["distance"] = json.Number("1e999")
			case "score_rounding":
				row["distance"] = 1.00001
				success = true
			case "zero_revision":
				row["publication_revision"] = "0"
				success = true
			}

			hits, err := f.base.client.SearchPrepared(mvContext(t), f.query)
			if !success {
				vsFail(t, hits, err, ErrResponse)
				return
			}
			if err != nil || hits == nil {
				t.Fatalf("valid response rejected: %v", err)
			}
			switch mode {
			case "empty":
				if len(hits) != 0 {
					t.Fatal("empty response produced hits")
				}
			case "score_rounding":
				if len(hits) != 2 || hits[0].Score != 1 {
					t.Fatal("cosine boundary was not normalized")
				}
			case "zero_revision":
				if len(hits) != 2 || hits[0].PublicationRevision != 0 {
					t.Fatal("zero publication revision changed")
				}
			}
		})
	}
}

func TestVectorSearchCancellation(t *testing.T) {
	for _, mode := range []string{"before", "during", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			f := vsNew(t)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			want := error(context.Canceled)
			if mode == "before" {
				cancel()
			}
			if mode == "deadline" {
				want = context.DeadlineExceeded
			}
			f.before = func(r *http.Request, path string) error {
				if path != "entities/search" {
					return nil
				}
				if mode == "during" {
					cancel()
				}
				<-r.Context().Done()
				return r.Context().Err()
			}
			hits, err := f.base.client.SearchPrepared(ctx, f.query)
			vsFail(t, hits, err, want)
			if mode == "before" && len(f.base.trace) != 0 {
				t.Fatal("pre-canceled search performed HTTP")
			}
			if mode != "before" &&
				f.base.trace[len(f.base.trace)-1] != "entities/search" {
				t.Fatal("search continued after cancellation")
			}
		})
	}
}

func TestVectorSearcherConstruction(t *testing.T) {
	for _, mode := range []string{
		"nil_embedder", "nil_client", "uninitialized_client", "zero_timeout", "negative_timeout",
	} {
		t.Run(mode, func(t *testing.T) {
			f := vsNew(t)
			var embedder QueryEmbedder = vqEmbedder(func(
				context.Context, []string,
			) (embedding.BatchResult, error) {
				t.Fatal("constructor called embedder")
				return embedding.BatchResult{}, nil
			})
			client := f.base.client
			timeout := time.Second
			switch mode {
			case "nil_embedder":
				embedder = nil
			case "nil_client":
				client = nil
			case "uninitialized_client":
				client = &Client{}
			case "zero_timeout":
				timeout = 0
			case "negative_timeout":
				timeout = -time.Second
			}
			got, err := NewSearcher(embedder, client, timeout)
			if got != nil || !errors.Is(err, ErrInput) || len(f.base.trace) != 0 {
				t.Fatal("invalid constructor input accepted or performed HTTP")
			}
		})
	}
}

func TestVectorSearcher(t *testing.T) {
	for _, mode := range []string{
		"mapping", "empty", "embedding_error", "search_error",
		"invalid_query", "nil_receiver", "shared_deadline", "parent_deadline",
	} {
		t.Run(mode, func(t *testing.T) {
			f := vsNew(t)
			embeddingCalls := 0
			var embeddingDeadline time.Time
			embedder := vqEmbedder(func(
				ctx context.Context, texts []string,
			) (embedding.BatchResult, error) {
				embeddingCalls++
				if !reflect.DeepEqual(texts, []string{"中文问题"}) {
					t.Fatal("searcher changed query")
				}
				var ok bool
				embeddingDeadline, ok = ctx.Deadline()
				if !ok {
					t.Fatal("embedding has no overall deadline")
				}
				if mode == "embedding_error" {
					return embedding.BatchResult{}, embedding.ErrUnavailable
				}
				return vqBatch(), nil
			})
			s, err := NewSearcher(embedder, f.base.client, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if len(f.base.trace) != 0 || embeddingCalls != 0 {
				t.Fatal("constructor performed I/O")
			}
			ctx := context.Background()
			if mode == "parent_deadline" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 500*time.Millisecond)
				defer cancel()
			}
			query := " 中文问题 "
			want := error(nil)
			switch mode {
			case "empty":
				f.rows = []map[string]any{}
			case "embedding_error":
				want = embedding.ErrUnavailable
			case "search_error":
				f.base.alias = ""
				want = retrieval.ErrIndexNotReady
			case "invalid_query":
				query = ""
				want = retrieval.ErrInvalidQuery
			case "nil_receiver":
				s = nil
				want = ErrInput
			case "shared_deadline", "parent_deadline":
				want = context.DeadlineExceeded
				f.before = func(r *http.Request, _ string) error {
					deadline, ok := r.Context().Deadline()
					if !ok || !deadline.Equal(embeddingDeadline) {
						t.Fatal("Milvus did not inherit embedding overall deadline")
					}
					<-r.Context().Done()
					return r.Context().Err()
				}
			}

			got, err := s.SearchCandidates(ctx, query, f.query.Options, f.query.At)
			if want != nil {
				if got != nil || !errors.Is(err, want) {
					t.Fatalf("got=%v err=%v want=%v", got, err, want)
				}
			} else {
				if err != nil || got == nil {
					t.Fatalf("got=%v err=%v", got, err)
				}
				if mode == "empty" {
					if len(got) != 0 {
						t.Fatal("empty search produced candidates")
					}
				} else {
					if len(got) != 2 {
						t.Fatal("candidate count differs")
					}
					for i, candidate := range got {
						wantCandidate := retrieval.Candidate{
							PublicationRevision: 2,
							Chunk: retrieval.RuleChunk{
								ChunkID: uint64(i + 1), DocumentID: 10,
								VersionID: 20, ArticleID: uint64(100 + i),
								CategoryID: 30, Version: "test-v1",
								EffectiveAt: time.Date(2026, 10, 31, 23, 0, 0, 0, time.UTC),
								Score:       0.9 - float64(i)*0.1,
								Highlights:  map[string][]string{},
							},
						}
						if !reflect.DeepEqual(candidate, wantCandidate) {
							t.Fatalf("candidate mapping differs: %+v", candidate)
						}
					}
				}
			}
			if mode == "invalid_query" || mode == "nil_receiver" {
				if embeddingCalls != 0 || len(f.base.trace) != 0 {
					t.Fatal("invalid request reached dependencies")
				}
			} else if embeddingCalls != 1 {
				t.Fatal("embedding was repeated")
			}
			if mode == "embedding_error" && len(f.base.trace) != 0 {
				t.Fatal("embedding failure still called Milvus")
			}
		})
	}
}
