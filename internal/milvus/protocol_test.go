package milvus

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"video-support-agent/internal/embedding"
	indexing "video-support-agent/internal/index"
	"video-support-agent/internal/vectorization"
)

func TestMilvusPrepareImport(t *testing.T) {
	makeSource := func() *vectorization.Snapshot {
		p := mvPrepared(2)
		s := &vectorization.Snapshot{
			SourceDatabase: p.SourceDatabase, SourceServerUUID: p.SourceServerUUID,
			CapturedAt: p.CapturedAt, Identity: p.Identity,
			Chunks: make([]vectorization.Chunk, 2),
		}
		for i, record := range p.Records {
			s.Chunks[i] = vectorization.Chunk{
				Source: indexing.Document{
					ChunkID: record.ChunkID, DocumentID: record.DocumentID,
					VersionID: record.VersionID, ArticleID: record.ArticleID,
					CategoryID:          record.Category,
					PublicationRevision: record.PublicationRevision,
					Version:             record.Version, ArticleOrder: uint32(i + 1),
					Title: "虚构测试", Content: mvSecret,
					SourceURL: "https://example.invalid/test",
					Status:    "published", IsCurrent: true,
					EffectiveAt: time.UnixMilli(record.EffectiveAt),
				},
				Vector: record.Embedding,
			}
		}
		expires := p.CapturedAt.Add(time.Hour)
		s.Chunks[1].Source.ExpiresAt = &expires
		return s
	}

	t.Run("mapping_and_copy", func(t *testing.T) {
		source := makeSource()
		got, err := PrepareImport(mvContext(t), source, mvIdentity().Digest)
		if err != nil {
			t.Fatal(err)
		}
		want := mvPrepared(2)
		want.Records[1].HasExpiresAt = true
		want.Records[1].ExpiresAt = source.Chunks[1].Source.ExpiresAt.UnixMilli()
		if !reflect.DeepEqual(got, want) {
			t.Fatal("metadata, time or vector mapping differs")
		}
		for _, record := range got.Records {
			var fields map[string]json.RawMessage
			mvDecode(t, mvJSON(t, record), &fields)
			if len(fields) != 11 {
				t.Fatal("record must contain exactly 11 fields")
			}
			for _, name := range mvFields {
				if _, ok := fields[name]; !ok {
					t.Fatalf("missing field %s", name)
				}
			}
			if strings.Contains(string(mvJSON(t, record)), mvSecret) {
				t.Fatal("full content leaked into Milvus record")
			}
		}
		source.Chunks[0].Vector[0] = 9
		if got.Records[0].Embedding[0] != 1 {
			t.Fatal("import vector aliases source")
		}
	})

	for _, mode := range []string{
		"nil", "empty", "wrong_digest", "stale", "duplicate",
		"version_bytes", "short_vector", "zero_vector", "nan",
	} {
		t.Run(mode, func(t *testing.T) {
			source := makeSource()
			var want error = ErrInput
			switch mode {
			case "nil":
				source = nil
			case "empty":
				source.Chunks = nil
			case "wrong_digest":
				source.Identity.Digest = strings.Repeat("b", 64)
				want = embedding.ErrModelMismatch
			case "stale":
				source.Chunks[1].Source.IsCurrent = false
				want = indexing.ErrInvalidSource
			case "duplicate":
				source.Chunks[1].Source.ChunkID = "1"
				want = indexing.ErrInvalidSource
			case "version_bytes":
				source.Chunks[1].Source.Version = strings.Repeat("版", 86)
			case "short_vector":
				source.Chunks[1].Vector = make([]float32, 1023)
			case "zero_vector":
				source.Chunks[1].Vector[0] = 0
			case "nan":
				source.Chunks[1].Vector[0] = float32(math.NaN())
			}
			got, err := PrepareImport(mvContext(t), source, mvIdentity().Digest)
			mvRequireError(t, err, want)
			if got != nil {
				t.Fatal("invalid import returned partial records")
			}
		})
	}
}

func TestMilvusBuildPreflight(t *testing.T) {
	for _, mode := range []string{
		"no_deadline", "nil_import", "empty", "bad_name", "duplicate_last",
		"bad_id", "bad_expiry", "bad_vector", "version_bytes",
	} {
		t.Run(mode, func(t *testing.T) {
			f := mvNewFixture(t, 65)
			ctx := mvContext(t)
			name := mvTarget
			switch mode {
			case "no_deadline":
				ctx = context.Background()
			case "nil_import":
				f.prepared = nil
			case "empty":
				f.prepared.Records = nil
			case "bad_name":
				name = mvAlias
			case "duplicate_last":
				f.prepared.Records[64].ChunkID = "1"
			case "bad_id":
				f.prepared.Records[64].DocumentID = "010"
			case "bad_expiry":
				f.prepared.Records[64].ExpiresAt = 1
			case "bad_vector":
				f.prepared.Records[64].Embedding[0] = 0
			case "version_bytes":
				f.prepared.Records[64].Version = strings.Repeat("版", 86)
			}
			got, err := f.client.BuildCollection(ctx, name, f.prepared)
			mvRequireError(t, err, ErrInput)
			if got == nil || got.Stage != "preflight" || len(f.trace) != 0 {
				t.Fatal("invalid import reached network")
			}
		})
	}
}

func TestMilvusBuildProtocol(t *testing.T) {
	f := mvNewFixture(t, 65)
	got, err := f.client.BuildCollection(mvContext(t), mvTarget, f.prepared)
	if err != nil {
		t.Fatal(err)
	}
	mvStageError(t, got, "ready-for-verification")
	if got.CollectionID != 123 || got.Inserted != 65 {
		t.Fatal("wrong build identity/count")
	}
	want := []string{
		"collections/list", "aliases/list", "collections/create",
		"collections/describe", "entities/insert", "entities/insert",
		"collections/flush", "indexes/create", "indexes/describe",
		"collections/load", "collections/get_load_state",
	}
	if !reflect.DeepEqual(f.trace, want) || len(f.inserted) != 65 {
		t.Fatalf("unexpected build sequence: %v", f.trace)
	}
	if f.alias != "old_target" {
		t.Fatal("build published without verification")
	}

	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		name, err := NewCollectionName()
		if err != nil || !targetPattern.MatchString(name) || seen[name] {
			t.Fatal("invalid or repeated generated name")
		}
		seen[name] = true
	}
}

func TestMilvusBuildFailures(t *testing.T) {
	tests := []struct {
		name  string
		path  string
		stage string
		want  error
	}{
		{"existing_target", "collections/list", "preflight", ErrInput},
		{"create_transport", "collections/create", "create", ErrUnavailable},
		{"schema", "collections/describe", "schema-check", ErrResponse},
		{"insert_count", "entities/insert", "insert", ErrResponse},
		{"insert_ids", "entities/insert", "insert", ErrResponse},
		{"flush", "collections/flush", "flush", ErrUnavailable},
		{"index_failed", "indexes/describe", "index", ErrRejected},
		{"index_metric", "indexes/describe", "index", ErrResponse},
		{"load_state", "collections/get_load_state", "load", ErrResponse},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := mvNewFixture(t, 2)
			f.hook = func(path string, _ map[string]json.RawMessage, data any) (any, error) {
				if path != tt.path {
					return data, nil
				}
				switch tt.name {
				case "existing_target":
					return []string{mvTarget}, nil
				case "create_transport", "flush":
					return nil, errors.New(mvSecret)
				case "schema":
					data.(map[string]any)["autoId"] = true
				case "insert_count":
					data.(map[string]any)["insertCount"] = 0
				case "insert_ids":
					data.(map[string]any)["insertIds"] = []string{"1", "1"}
				case "index_failed":
					data.([]map[string]any)[0]["indexState"] = "Failed"
				case "index_metric":
					data.([]map[string]any)[0]["metricType"] = "L2"
				case "load_state":
					data.(map[string]any)["loadState"] = "unexpected"
				}
				return data, nil
			}
			got, err := f.client.BuildCollection(mvContext(t), mvTarget, f.prepared)
			mvRequireError(t, err, tt.want)
			mvStageError(t, got, tt.stage)
			if f.calls[tt.path] != 1 || f.trace[len(f.trace)-1] != tt.path {
				t.Fatal("failure retried or pipeline continued")
			}
			if f.alias != "old_target" {
				t.Fatal("failed build published")
			}
			if tt.name == "create_transport" || tt.name == "flush" ||
				strings.HasPrefix(tt.name, "insert_") {
				if !errors.Is(err, ErrStateUnknown) {
					t.Fatal("mutation uncertainty was lost")
				}
			}
		})
	}
}

func TestMilvusSchema(t *testing.T) {
	for _, mode := range []string{
		"missing_boolean", "dynamic", "wrong_description", "wrong_id",
		"missing_field", "duplicate_field", "nullable", "dimension", "primary",
		"primary_auto_missing", "primary_auto_true", "non_primary_auto_true",
	} {
		t.Run(mode, func(t *testing.T) {
			f := mvNewFixture(t, 1)
			f.hook = func(path string, _ map[string]json.RawMessage, data any) (any, error) {
				if path != "collections/describe" {
					return data, nil
				}
				d := data.(map[string]any)
				fields := d["fields"].([]map[string]any)
				switch mode {
				case "missing_boolean":
					delete(d, "autoId")
				case "dynamic":
					d["enableDynamicField"] = true
				case "wrong_description":
					d["description"] = "{}"
				case "wrong_id":
					d["collectionID"] = 0
				case "missing_field":
					d["fields"] = fields[:10]
				case "duplicate_field":
					fields[1] = fields[0]
				case "nullable":
					fields[0]["nullable"] = true
				case "dimension":
					fields[7]["params"] = []map[string]string{{"key": "dim", "value": "768"}}
				case "primary":
					fields[0]["primaryKey"] = false
				case "primary_auto_missing":
					delete(fields[0], "autoId")
				case "primary_auto_true":
					fields[0]["autoId"] = true
				case "non_primary_auto_true":
					fields[1]["autoId"] = true
				}
				return data, nil
			}
			id, err := f.client.VerifyCollectionContract(mvContext(t), mvTarget, mvIdentity())
			mvRequireError(t, err, ErrResponse)
			if id != 0 {
				t.Fatal("invalid schema returned usable collection ID")
			}
		})
	}
}

func TestMilvusVerification(t *testing.T) {
	t.Run("all_records_and_search", func(t *testing.T) {
		f := mvNewFixture(t, 65)
		token := f.token()
		report := token.Report()
		if report.Collection != mvTarget || report.CollectionID != 123 ||
			report.Count != 65 || report.Identity != mvIdentity() {
			t.Fatal("invalid verification report")
		}
		report.Count = 999
		if token.Report().Count != 65 {
			t.Fatal("Report exposed mutable token state")
		}
		// token()清理请求计数，下面用新夹具明确检查完整查询次数。
		g := mvNewFixture(t, 65)
		_, err := g.client.VerifyCollection(mvContext(t), &BuildResult{
			Collection: mvTarget, CollectionID: 123,
			Stage: "ready-for-verification", Inserted: 65,
		}, g.prepared)
		if err != nil {
			t.Fatal(err)
		}
		if g.calls["entities/query"] != 4 ||
			g.calls["collections/describe"] != 2 ||
			!reflect.DeepEqual(g.searchIDs, []string{"1", "33", "65"}) {
			t.Fatal("full query batches, boundary checks or self-search missing")
		}
	})

	modes := append([]string(nil), mvFields...)
	modes = append(modes,
		"missing_row", "duplicate_row", "extra_field", "null_component",
		"search_score", "search_id", "final_count", "final_collection_id",
		"search_missing_chunk", "search_null_chunk", "search_numeric_chunk",
		"search_missing_distance", "search_null_distance", "search_empty",
		"search_only_id",
	)
	for _, mode := range modes {
		t.Run(mode, func(t *testing.T) {
			f := mvNewFixture(t, 3)
			f.hook = func(path string, req map[string]json.RawMessage, data any) (any, error) {
				if mode == "final_collection_id" && path == "collections/describe" &&
					f.calls[path] == 2 {
					data.(map[string]any)["collectionID"] = 124
				}
				if path == "entities/search" {
					hits := data.([]map[string]any)
					switch mode {
					case "search_score":
						hits[0]["distance"] = 0.5
					case "search_id":
						hits[0]["chunk_id"] = "999"
					case "search_missing_chunk":
						delete(hits[0], "chunk_id")
					case "search_null_chunk":
						hits[0]["chunk_id"] = nil
					case "search_numeric_chunk":
						hits[0]["chunk_id"] = 1
					case "search_missing_distance":
						delete(hits[0], "distance")
					case "search_null_distance":
						hits[0]["distance"] = nil
					case "search_empty":
						return []map[string]any{}, nil
					case "search_only_id":
						hits[0]["id"] = hits[0]["chunk_id"]
						delete(hits[0], "chunk_id")
					}
				}
				if path != "entities/query" {
					return data, nil
				}
				var filter string
				mvDecode(t, req["filter"], &filter)
				if filter == "" {
					if mode == "final_count" && f.calls[path] == 3 {
						data.([]map[string]any)[0]["count(*)"] = 99
					}
					return data, nil
				}
				rows := data.([]map[string]any)
				switch mode {
				case "missing_row":
					return rows[:2], nil
				case "duplicate_row":
					rows[1] = rows[0]
				case "extra_field":
					rows[0]["content"] = mvSecret
				case "null_component":
					rows[0]["embedding"].([]any)[10] = nil
				case "embedding":
					rows[0]["embedding"].([]any)[0] = 0.5
				case "effective_at", "expires_at":
					rows[0][mode] = rows[0][mode].(float64) + 1
				case "has_expires_at":
					rows[0][mode] = true
				default:
					for _, name := range mvFields {
						if mode == name {
							rows[0][mode] = "999"
						}
					}
				}
				return rows, nil
			}
			token, err := f.client.VerifyCollection(mvContext(t), &BuildResult{
				Collection: mvTarget, CollectionID: 123,
				Stage: "ready-for-verification", Inserted: 3,
			}, f.prepared)
			mvRequireError(t, err, ErrResponse)
			if token != nil || f.alias != "old_target" {
				t.Fatal("failed verification returned token or published")
			}
		})
	}
}

func TestMilvusPublication(t *testing.T) {
	for _, mode := range []string{
		"alter", "create", "write_error_but_applied", "old_observed",
		"third_target", "observation_error", "cancel_after_write",
		"old_changed", "collection_changed", "foreign_token", "nil_token",
	} {
		t.Run(mode, func(t *testing.T) {
			f := mvNewFixture(t, 1)
			token := f.token()
			ctx, cancel := context.WithCancel(mvContext(t))
			defer cancel()
			expectedOld := "old_target"
			if mode == "create" {
				f.alias = ""
				expectedOld = ""
			}
			if mode == "old_changed" {
				f.alias = "another_target"
			}
			if mode == "foreign_token" {
				token.client = mvClient(t)
			}
			if mode == "nil_token" {
				token = nil
			}
			written := false
			f.hook = func(path string, _ map[string]json.RawMessage, data any) (any, error) {
				if mode == "collection_changed" && path == "collections/describe" {
					data.(map[string]any)["collectionID"] = 124
				}
				if written && path == "aliases/list" && mode == "observation_error" {
					return nil, errors.New(mvSecret)
				}
				if path != "aliases/create" && path != "aliases/alter" {
					return data, nil
				}
				written = true
				switch mode {
				case "write_error_but_applied":
					return nil, errors.New(mvSecret)
				case "old_observed":
					f.alias = expectedOld
				case "third_target":
					f.alias = "third_target"
				case "cancel_after_write":
					cancel()
					return nil, context.Canceled
				}
				return data, nil
			}
			got, err := f.client.PublishVerified(ctx, expectedOld, token)
			writes := f.calls["aliases/create"] + f.calls["aliases/alter"]
			switch mode {
			case "old_changed", "collection_changed", "foreign_token", "nil_token":
				if err == nil || writes != 0 || got.State != PublicationNotAttempted {
					t.Fatalf("invalid publication was attempted: %+v %v", got, err)
				}
			case "alter", "create":
				if err != nil || writes != 1 || got.State != PublicationApplied ||
					!got.RequestAcknowledged || !got.ObservationSucceeded ||
					got.ObservedTarget != mvTarget {
					t.Fatalf("publication failed: %+v %v", got, err)
				}
				path := "aliases/alter"
				if mode == "create" {
					path = "aliases/create"
				}
				if f.calls[path] != 1 {
					t.Fatal("wrong publication endpoint")
				}
			case "write_error_but_applied", "cancel_after_write":
				want := ErrStateUnknown
				if mode == "cancel_after_write" {
					want = context.Canceled
				}
				mvRequireError(t, err, want)
				if writes != 1 || got.State != PublicationApplied ||
					got.RequestAcknowledged || !got.ObservationSucceeded ||
					got.RequestError == "" {
					t.Fatalf("lost uncertainty/observation: %+v", got)
				}
			default:
				mvRequireError(t, err, ErrStateUnknown)
				want := PublicationUnknown
				if mode == "old_observed" {
					want = PublicationOldObserved
				}
				if writes != 1 || got.State != want ||
					!got.RequestAcknowledged || f.calls["aliases/list"] != 4 {
					t.Fatalf("wrong bounded observation: %+v calls=%v", got, f.calls)
				}
				if mode == "observation_error" &&
					(got.ObservationSucceeded || got.ObservationError == "") {
					t.Fatal("observation failure was hidden")
				}
			}
			if token != nil && token.client == f.client {
				before := len(f.trace)
				again, retryErr := f.client.PublishVerified(mvContext(t), expectedOld, token)
				mvRequireError(t, retryErr, ErrInput)
				if again.State != PublicationNotAttempted || len(f.trace) != before {
					t.Fatal("consumed token caused another request")
				}
			}
		})
	}
}

type mvTrackedBody struct {
	io.Reader
	closed bool
}

func (b *mvTrackedBody) Close() error {
	b.closed = true
	return nil
}

func TestMilvusRequest(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"business_error", 200, `{"code":1804,"message":"` + mvSecret + `"}`, ErrRejected},
		{"missing_code", 200, `{"data":{}}`, ErrResponse},
		{"null_code", 200, `{"code":null,"data":{}}`, ErrResponse},
		{"string_code", 200, `{"code":"0","data":{}}`, ErrResponse},
		{"missing_data", 200, `{"code":0}`, ErrResponse},
		{"null_data", 200, `{"code":0,"data":null}`, ErrResponse},
		{"trailing_json", 200, `{"code":0,"data":{}}{}`, ErrResponse},
		{"invalid_utf8", 200, string([]byte{0xff}), ErrResponse},
		{"oversized", 200, strings.Repeat("x", (4<<20)+1), ErrResponse},
		{"server_error", 503, mvSecret, ErrUnavailable},
		{"rate_limit", 429, mvSecret, ErrUnavailable},
		{"rejected", 403, mvSecret, ErrRejected},
		{"redirect", 307, mvSecret, ErrRejected},
	}
	for _, tt := range tests {
		for _, mutation := range []bool{false, true} {
			suffix := "/read"
			if mutation {
				suffix = "/write"
			}
			t.Run(tt.name+suffix, func(t *testing.T) {
				client := mvClient(t)
				calls := 0
				body := &mvTrackedBody{Reader: strings.NewReader(tt.body)}
				client.http.Transport = mvTransport(func(*http.Request) (*http.Response, error) {
					calls++
					return &http.Response{
						StatusCode: tt.status, Header: make(http.Header), Body: body,
					}, nil
				})
				got, err := client.request(mvContext(t), "entities/insert", nil, mutation)
				mvRequireError(t, err, tt.want)
				if got != nil || calls != 1 || !body.closed {
					t.Fatal("partial result, retry or response body leak")
				}
				if errors.Is(err, ErrStateUnknown) != mutation {
					t.Fatal("incorrect mutation uncertainty classification")
				}
				if tt.name == "business_error" {
					var apiError *APIError
					if !errors.As(err, &apiError) || apiError.Code != 1804 {
						t.Fatal("business code was lost")
					}
				}
			})
		}
	}

	t.Run("payload_copy_and_preflight", func(t *testing.T) {
		f := mvNewFixture(t, 1)
		payload := map[string]any{"dbName": "caller-value"}
		if _, err := f.client.request(mvContext(t), "collections/list", payload, false); err != nil {
			t.Fatal(err)
		}
		if payload["dbName"] != "caller-value" {
			t.Fatal("request mutated caller payload")
		}
		before := len(f.trace)
		for _, invalid := range []map[string]any{
			{"value": make(chan int)},
			{"value": strings.Repeat("x", 2<<20)},
		} {
			_, err := f.client.request(mvContext(t), "collections/create", invalid, true)
			mvRequireError(t, err, ErrInput)
			if errors.Is(err, ErrStateUnknown) {
				t.Fatal("local rejection marked as dispatched mutation")
			}
		}
		if len(f.trace) != before {
			t.Fatal("invalid payload reached transport")
		}
	})
}

func TestMilvusCancellation(t *testing.T) {
	for _, mode := range []string{"before_request", "during_request", "timeout", "poll"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(mvContext(t))
			defer cancel()
			client := mvClient(t)
			calls := 0
			if mode == "poll" {
				err := poll(ctx, func() (bool, error) {
					calls++
					cancel()
					return false, nil
				})
				mvRequireError(t, err, context.Canceled)
				if calls != 1 {
					t.Fatal("canceled poll continued")
				}
				return
			}
			client.http.Transport = mvTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if mode == "during_request" {
					cancel()
				}
				<-r.Context().Done()
				return nil, r.Context().Err()
			})
			if mode == "before_request" {
				cancel()
			}
			if mode == "timeout" {
				client.timeout = 25 * time.Millisecond
			}
			_, err := client.request(ctx, "collections/create", nil, true)
			want := context.Canceled
			if mode == "timeout" {
				want = context.DeadlineExceeded
			}
			mvRequireError(t, err, want)
			expectedCalls := 1
			if mode == "before_request" {
				expectedCalls = 0
			}
			if calls != expectedCalls ||
				errors.Is(err, ErrStateUnknown) != (expectedCalls == 1) {
				t.Fatal("cancellation lost dispatch/uncertainty boundary")
			}
		})
	}
}
