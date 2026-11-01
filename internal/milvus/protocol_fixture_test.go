package milvus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"video-support-agent/internal/config"
	"video-support-agent/internal/embedding"
)

const mvTarget = "rule_chunks_vec_v1_0123456789abcdef0123456789abcdef"
const mvAlias = "milvus_protocol_test"
const mvSecret = "PRIVATE_RULE_CONTENT_92741"

var mvFields = []string{
	"chunk_id", "document_id", "version_id", "article_id",
	"category", "publication_revision", "version", "embedding",
	"effective_at", "has_expires_at", "expires_at",
}

type mvTransport func(*http.Request) (*http.Response, error)

func (f mvTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func mvJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func mvDecode(t *testing.T, data []byte, target any) {
	t.Helper()
	if err := json.Unmarshal(data, target); err != nil {
		t.Fatalf("decode fixture/request: %v", err)
	}
}

func mvMatch(t *testing.T, actual json.RawMessage, expected any) {
	t.Helper()
	var got, want any
	mvDecode(t, actual, &got)
	mvDecode(t, mvJSON(t, expected), &want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("JSON differs: got=%v want=%v", got, want)
	}
}

func mvContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func mvClient(t *testing.T) *Client {
	t.Helper()
	client, err := NewClient(config.MilvusConfig{
		URL:             "http://127.0.0.1:1",
		Database:        "default",
		CollectionAlias: mvAlias,
		Timeout:         "2s",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	return client
}

func mvIdentity() embedding.ModelIdentity {
	return embedding.ModelIdentity{
		Model: embedding.ModelName, Digest: strings.Repeat("a", 64),
		ServerVersion: "0.40.13", Dimensions: 1024,
		ContextLength: 8192, InputPolicy: embedding.InputPolicy,
	}
}

func mvPrepared(count int) *PreparedImport {
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	p := &PreparedImport{
		SourceDatabase: "test_source", SourceServerUUID: "test-server",
		CapturedAt: at, Identity: mvIdentity(),
		Records: make([]Record, count),
	}
	for i := range p.Records {
		vector := make([]float32, 1024)
		vector[0] = 1
		p.Records[i] = Record{
			ChunkID: strconv.Itoa(i + 1), DocumentID: "10",
			VersionID: "20", ArticleID: strconv.Itoa(100 + i),
			Category: "30", PublicationRevision: "2", Version: "test-v1",
			Embedding: vector, EffectiveAt: at.Add(-time.Hour).UnixMilli(),
		}
	}
	return p
}

func mvDescription(t *testing.T, identity embedding.ModelIdentity) string {
	t.Helper()
	return `{"contract_version":1,"identity":` + string(mvJSON(t, identity)) + `}`
}

// 独立描述预期字段，不调用生产代码的 collectionFields。
func mvFieldDefinitions(create bool) []map[string]any {
	fields := make([]map[string]any, 0, 11)
	for i, name := range mvFields {
		kind := "VarChar"
		params := map[string]string{}
		switch {
		case i < 6:
			params["max_length"] = "20"
		case name == "version":
			params["max_length"] = "256"
		case name == "embedding":
			kind = "FloatVector"
			params["dim"] = "1024"
		case name == "has_expires_at":
			kind = "Bool"
		default:
			kind = "Int64"
		}
		if create {
			field := map[string]any{
				"fieldName": name, "dataType": kind,
				"isPrimary": name == "chunk_id", "nullable": false,
			}
			if len(params) != 0 {
				field["elementTypeParams"] = params
			}
			fields = append(fields, field)
		} else {
			list := make([]map[string]string, 0, len(params))
			for key, value := range params {
				list = append(list, map[string]string{"key": key, "value": value})
			}
			field := map[string]any{
				"name":       name,
				"type":       kind,
				"primaryKey": name == "chunk_id",
				"nullable":   false,
				"params":     list,
			}
			if name == "chunk_id" {
				field["autoId"] = false
			}
			fields = append(fields, field)
		}
	}
	return fields
}

type mvFixture struct {
	t         *testing.T
	client    *Client
	prepared  *PreparedImport
	alias     string
	trace     []string
	calls     map[string]int
	inserted  []Record
	searchIDs []string
	hook      func(string, map[string]json.RawMessage, any) (any, error)
}

func mvNewFixture(t *testing.T, count int) *mvFixture {
	t.Helper()
	f := &mvFixture{
		t: t, client: mvClient(t), prepared: mvPrepared(count),
		alias: "old_target", calls: make(map[string]int),
	}
	f.client.http.Transport = mvTransport(f.roundTrip)
	return f
}

func (f *mvFixture) roundTrip(r *http.Request) (*http.Response, error) {
	t := f.t
	if r.Method != "POST" ||
		r.Header.Get("Content-Type") != "application/json" ||
		r.Header.Get("Accept") != "application/json" {
		t.Fatal("invalid HTTP request contract")
	}
	path := strings.TrimPrefix(r.URL.Path, "/v2/vectordb/")
	if path == r.URL.Path || r.URL.RawQuery != "" {
		t.Fatal("invalid REST endpoint")
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	if len(raw) > 2<<20 {
		t.Fatal("encoded request exceeded 2 MiB")
	}
	var req map[string]json.RawMessage
	mvDecode(t, raw, &req)
	mvMatch(t, req["dbName"], "default")
	if value, ok := req["collectionName"]; ok {
		mvMatch(t, value, mvTarget)
	}
	if value, ok := req["aliasName"]; ok {
		mvMatch(t, value, mvAlias)
	}
	if _, ok := r.Context().Deadline(); !ok {
		t.Fatal("HTTP request missing deadline")
	}
	f.trace = append(f.trace, path)
	f.calls[path]++

	data := f.reply(path, req)
	if f.hook != nil {
		data, err = f.hook(path, req, data)
		if err != nil {
			return nil, err
		}
	}
	body := mvJSON(t, map[string]any{"code": 0, "data": data})
	return &http.Response{
		StatusCode: 200, Header: make(http.Header),
		Body: io.NopCloser(strings.NewReader(string(body))),
	}, nil
}

func (f *mvFixture) reply(path string, req map[string]json.RawMessage) any {
	t := f.t
	switch path {
	case "collections/list":
		return []string{}
	case "aliases/list":
		if f.alias == "" {
			return []string{}
		}
		return []string{mvAlias}
	case "aliases/describe":
		return map[string]any{
			"aliasName": mvAlias, "collectionName": f.alias, "dbName": "default",
		}
	case "collections/create":
		mvMatch(t, req["description"], mvDescription(t, f.prepared.Identity))
		mvMatch(t, req["schema"], map[string]any{
			"autoId": false, "enabledDynamicField": false,
			"fields": mvFieldDefinitions(true),
		})
		mvMatch(t, req["params"], map[string]any{
			"consistencyLevel": "Strong", "shardsNum": 1,
		})
		return map[string]any{}
	case "collections/describe":
		return map[string]any{
			"collectionName": mvTarget, "collectionID": int64(123),
			"description": mvDescription(t, f.prepared.Identity),
			"autoId":      false, "enableDynamicField": false,
			"consistencyLevel": "Strong", "shardsNum": 1,
			"fields": mvFieldDefinitions(false),
		}
	case "entities/insert":
		var records []Record
		mvDecode(t, req["data"], &records)
		if len(records) == 0 || len(records) > 64 {
			t.Fatal("invalid insert batch size")
		}
		start := len(f.inserted)
		if start+len(records) > len(f.prepared.Records) ||
			!reflect.DeepEqual(records, f.prepared.Records[start:start+len(records)]) {
			t.Fatal("insert records changed or reordered")
		}
		f.inserted = append(f.inserted, records...)
		ids := make([]string, len(records))
		for i := range records {
			// ID 确认是集合比较，不要求服务端按提交顺序返回。
			ids[len(records)-1-i] = records[i].ChunkID
		}
		return map[string]any{"insertCount": len(records), "insertIds": ids}
	case "collections/flush":
		if _, exists := req["collectionNames"]; exists {
			t.Fatal("flush must use singular collectionName")
		}
		return map[string]any{}
	case "indexes/create":
		mvMatch(t, req["indexParams"], []map[string]any{{
			"fieldName": "embedding", "indexName": "embedding_hnsw",
			"indexType": "HNSW", "metricType": "COSINE",
			"params": map[string]string{"M": "16", "efConstruction": "200"},
		}})
		return map[string]any{}
	case "indexes/describe":
		mvMatch(t, req["indexName"], "embedding_hnsw")
		return []map[string]any{{
			"fieldName": "embedding", "indexName": "embedding_hnsw",
			"indexType": "HNSW", "metricType": "COSINE", "indexState": "Finished",
			"indexParams": []map[string]string{{
				"key": "params", "value": `{"M":"16","efConstruction":"200"}`,
			}},
		}}
	case "collections/load":
		return map[string]any{}
	case "collections/get_load_state":
		return map[string]any{"loadState": "LoadStateLoaded"}
	case "entities/query":
		mvMatch(t, req["consistencyLevel"], "Strong")
		var filter string
		mvDecode(t, req["filter"], &filter)
		if filter == "" {
			mvMatch(t, req["outputFields"], []string{"count(*)"})
			return []map[string]any{{"count(*)": len(f.prepared.Records)}}
		}
		mvMatch(t, req["outputFields"], mvFields)
		if !strings.HasPrefix(filter, "chunk_id in ") {
			t.Fatal("invalid query filter")
		}
		var ids []string
		mvDecode(t, []byte(strings.TrimPrefix(filter, "chunk_id in ")), &ids)
		mvMatch(t, req["limit"], len(ids))
		if len(ids) == 0 || len(ids) > 64 {
			t.Fatal("invalid query batch size")
		}
		rows := make([]map[string]any, 0, len(ids))
		for i := len(ids) - 1; i >= 0; i-- {
			n, err := strconv.Atoi(ids[i])
			if err != nil || n < 1 || n > len(f.prepared.Records) {
				t.Fatal("unexpected query ID")
			}
			var row map[string]any
			mvDecode(t, mvJSON(t, f.prepared.Records[n-1]), &row)
			rows = append(rows, row)
		}
		return rows
	case "entities/search":
		mvMatch(t, req["annsField"], "embedding")
		mvMatch(t, req["limit"], 1)
		mvMatch(t, req["outputFields"], []string{"chunk_id"})
		mvMatch(t, req["consistencyLevel"], "Strong")
		mvMatch(t, req["searchParams"], map[string]any{
			"metricType": "COSINE", "params": map[string]any{"ef": 64},
		})
		var filter, id string
		mvDecode(t, req["filter"], &filter)
		if !strings.HasPrefix(filter, "chunk_id == ") {
			t.Fatal("invalid search filter")
		}
		mvDecode(t, []byte(strings.TrimPrefix(filter, "chunk_id == ")), &id)
		n, err := strconv.Atoi(id)
		if err != nil || n < 1 || n > len(f.prepared.Records) {
			t.Fatal("invalid search ID")
		}
		mvMatch(t, req["data"], [][]float32{f.prepared.Records[n-1].Embedding})
		f.searchIDs = append(f.searchIDs, id)
		return []map[string]any{{"chunk_id": id, "distance": 1.0}}
	case "aliases/create", "aliases/alter":
		f.alias = mvTarget
		return map[string]any{}
	default:
		t.Fatalf("unexpected endpoint, including retry/delete: %s", path)
		return nil
	}
}

func (f *mvFixture) token() *VerifiedTarget {
	f.t.Helper()
	token, err := f.client.VerifyCollection(mvContext(f.t), &BuildResult{
		Collection: mvTarget, CollectionID: 123,
		Stage: "ready-for-verification", Inserted: len(f.prepared.Records),
	}, f.prepared)
	if err != nil {
		f.t.Fatal(err)
	}
	f.trace = nil
	f.calls = make(map[string]int)
	f.searchIDs = nil
	return token
}

func mvRequireError(t *testing.T, err, expected error) {
	t.Helper()
	if !errors.Is(err, expected) {
		t.Fatalf("error=%v, want=%v", err, expected)
	}
	if strings.Contains(err.Error(), mvSecret) {
		t.Fatal("error leaked server/transport content")
	}
}

// fmt 在失败时给出可读的阶段上下文。
func mvStageError(t *testing.T, result *BuildResult, stage string) {
	t.Helper()
	if result == nil || result.Stage != stage || result.Collection != mvTarget {
		t.Fatal(fmt.Sprintf("unexpected build result: %+v; want stage %s", result, stage))
	}
}
