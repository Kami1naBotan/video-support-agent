package elasticsearch

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	indexing "video-support-agent/internal/index"
)

func protocolDocuments(t *testing.T, count, contentBytes int) []indexing.EncodedDocument {
	t.Helper()

	documents := make([]indexing.EncodedDocument, count)
	for i := range documents {
		id := strconv.Itoa(i + 1)
		source := map[string]any{
			"chunk_id": id,
			"content":  strings.Repeat("x", contentBytes) + "\n中文",
		}
		body, err := json.MarshalIndent(source, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		documents[i] = indexing.EncodedDocument{ID: id, JSON: body}
	}
	return documents
}

func protocolBulkItems(ids []string) []any {
	items := make([]any, 0, len(ids))
	for _, id := range ids {
		items = append(items, map[string]any{
			"index": map[string]any{
				"_index": protocolIndex,
				"_id":    id,
				"status": 201,
				"result": "created",
				"_shards": map[string]any{
					"failed": 0,
				},
			},
		})
	}
	return items
}

func protocolIDs(documents []indexing.EncodedDocument) []string {
	ids := make([]string, len(documents))
	for i := range documents {
		ids[i] = documents[i].ID
	}
	return ids
}

func TestESProtocolBulkRealHTTPBatching(t *testing.T) {
	cases := []struct {
		name         string
		count        int
		contentBytes int
		batchSizes   []int
	}{
		{"count_limit", 501, 10, []int{500, 1}},
		{"byte_limit", 7, 900000, []int{4, 3}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			documents := protocolDocuments(t, tc.count, tc.contentBytes)
			var mu sync.Mutex
			var sizes []int
			var received []string
			guardReads := 0

			server := httptest.NewServer(http.HandlerFunc(
				func(w http.ResponseWriter, r *http.Request) {
					mu.Lock()
					defer mu.Unlock()

					if r.Method == http.MethodGet &&
						r.URL.Path == "/"+protocolIndex+"/_alias" {
						guardReads++
						io.WriteString(w, `{"test_new_index":{"aliases":{}}}`)
						return
					}
					if r.Method != http.MethodPost ||
						r.URL.Path != "/"+protocolIndex+"/_bulk" {
						t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
						http.Error(w, "unexpected", 400)
						return
					}
					if r.Header.Get("Content-Type") != "application/x-ndjson" {
						t.Error("incorrect bulk content type")
					}

					body, err := io.ReadAll(io.LimitReader(
						r.Body, int64(MaxRequestBytes)+1,
					))
					if err != nil || len(body) > MaxRequestBytes ||
						!bytes.HasSuffix(body, []byte("\n")) {
						t.Error("invalid bulk byte limit or final newline")
						http.Error(w, "invalid body", 400)
						return
					}

					lines := bytes.Split(body[:len(body)-1], []byte("\n"))
					if len(lines)%2 != 0 || len(lines)/2 > MaxBulkDocuments {
						t.Error("invalid NDJSON framing or document count")
						http.Error(w, "invalid framing", 400)
						return
					}

					var batchIDs []string
					for i := 0; i < len(lines); i += 2 {
						var action map[string]map[string]string
						if json.Unmarshal(lines[i], &action) != nil ||
							len(action) != 1 || len(action["index"]) != 1 {
							t.Error("invalid action line")
							http.Error(w, "invalid action", 400)
							return
						}
						id := action["index"]["_id"]
						position := len(received)
						if position >= len(documents) ||
							id != documents[position].ID {
							t.Error("bulk order or ID changed")
							http.Error(w, "invalid identity", 400)
							return
						}

						var compact bytes.Buffer
						if err := json.Compact(&compact, documents[position].JSON); err != nil || !bytes.Equal(compact.Bytes(), lines[i+1]) {
							t.Error("source changed or was not compacted")
							http.Error(w, "invalid source", 400)
							return
						}
						received = append(received, id)
						batchIDs = append(batchIDs, id)
					}

					sizes = append(sizes, len(batchIDs))
					err = json.NewEncoder(w).Encode(map[string]any{
						"errors": false,
						"items":  protocolBulkItems(batchIDs),
					})
					if err != nil {
						t.Errorf("write response: %v", err)
					}
				},
			))
			t.Cleanup(server.Close)
			client := protocolClient(t, server.URL)

			if err := client.BulkIndex(
				context.Background(), protocolIndex, documents,
			); err != nil {
				t.Fatal(err)
			}

			mu.Lock()
			defer mu.Unlock()
			if guardReads != 1 ||
				!reflect.DeepEqual(sizes, tc.batchSizes) ||
				!reflect.DeepEqual(received, protocolIDs(documents)) {
				t.Fatalf("unexpected batches: sizes=%v guardReads=%d",
					sizes, guardReads)
			}
		})
	}
}

func TestESProtocolBulkInputGuards(t *testing.T) {
	cases := []struct {
		name   string
		change func([]indexing.EncodedDocument) []indexing.EncodedDocument
	}{
		{"nil", func(d []indexing.EncodedDocument) []indexing.EncodedDocument {
			return nil
		}},
		{"duplicate", func(d []indexing.EncodedDocument) []indexing.EncodedDocument {
			return append(d, d[0])
		}},
		{"noncanonical", func(d []indexing.EncodedDocument) []indexing.EncodedDocument {
			d[0].ID = "01"
			return d
		}},
		{"id_mismatch", func(d []indexing.EncodedDocument) []indexing.EncodedDocument {
			d[0].ID = "2"
			return d
		}},
		{"null_source", func(d []indexing.EncodedDocument) []indexing.EncodedDocument {
			d[0].JSON = []byte("null")
			return d
		}},
		{"invalid_utf8", func(d []indexing.EncodedDocument) []indexing.EncodedDocument {
			d[0].JSON = []byte{0xff}
			return d
		}},
		{"too_large", func(d []indexing.EncodedDocument) []indexing.EncodedDocument {
			d[0].JSON = make([]byte, indexing.MaxIndexDocumentJSONBytes+1)
			return d
		}},
		{"too_many", func(d []indexing.EncodedDocument) []indexing.EncodedDocument {
			return make([]indexing.EncodedDocument, indexing.MaxExportDocuments+1)
		}},
		{"invalid_after_first_batch", func(d []indexing.EncodedDocument) []indexing.EncodedDocument {
			d = protocolDocuments(t, 501, 1)
			d[500].JSON = []byte("{")
			return d
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := protocolScript(t)
			documents := tc.change(protocolDocuments(t, 1, 1))
			requireProtocolError(t,
				client.BulkIndex(context.Background(), protocolIndex, documents),
				ErrRejected)
		})
	}

	t.Run("empty_only_checks_target", func(t *testing.T) {
		client := protocolScript(t, protocolUnaliasedStep())
		if err := client.BulkIndex(
			context.Background(), protocolIndex, []indexing.EncodedDocument{},
		); err != nil {
			t.Fatal(err)
		}
	})

	for _, body := range []string{
		`{"test_new_index":{"aliases":{"live":{}}}}`,
		`{"other":{"aliases":{}}}`,
		`{"test_new_index":{}}`,
	} {
		t.Run("target_guard", func(t *testing.T) {
			step := protocolUnaliasedStep()
			step.body = body
			client := protocolScript(t, step)
			err := client.BulkIndex(
				context.Background(), protocolIndex, protocolDocuments(t, 1, 1),
			)
			requireProtocolError(t, err)
		})
	}
}

func TestESProtocolBulkResponses(t *testing.T) {
	cases := []struct {
		name   string
		change func(map[string]any, map[string]any)
		want   error
	}{
		{"created", func(e, item map[string]any) {}, nil},
		{"updated", func(e, item map[string]any) {
			item["status"], item["result"] = 200, "updated"
		}, nil},
		{"missing_errors", func(e, item map[string]any) {
			delete(e, "errors")
		}, ErrResponse},
		{"wrong_count", func(e, item map[string]any) {
			e["items"] = []any{}
		}, ErrResponse},
		{"wrong_id", func(e, item map[string]any) {
			item["_id"] = "99"
		}, ErrResponse},
		{"wrong_index", func(e, item map[string]any) {
			item["_index"] = "another_index"
		}, ErrResponse},
		{"wrong_operation", func(e, item map[string]any) {
			e["items"] = []any{map[string]any{"create": item}}
		}, ErrResponse},
		{"wrong_result", func(e, item map[string]any) {
			item["result"] = "deleted"
		}, ErrResponse},
		{"missing_shards", func(e, item map[string]any) {
			delete(item, "_shards")
		}, ErrResponse},
		{"failed_shard", func(e, item map[string]any) {
			item["_shards"] = map[string]any{"failed": 1}
		}, ErrResponse},
		{"contradictory_errors", func(e, item map[string]any) {
			e["errors"] = true
		}, ErrResponse},
		{"explicit_failure", func(e, item map[string]any) {
			e["errors"] = true
			item["status"] = 400
			item["error"] = map[string]any{
				"type": "mapper_parsing_exception", "reason": protocolSecret,
			}
		}, ErrRejected},
		{"failure_without_details", func(e, item map[string]any) {
			e["errors"] = true
			item["status"] = 400
		}, ErrResponse},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			items := protocolBulkItems([]string{"1"})
			item := items[0].(map[string]any)["index"].(map[string]any)
			envelope := map[string]any{"errors": false, "items": items}
			tc.change(envelope, item)

			client := protocolScript(t,
				protocolUnaliasedStep(),
				protocolStep{
					method: http.MethodPost,
					path:   "/" + protocolIndex + "/_bulk",
					body:   protocolJSON(t, envelope),
				},
			)

			err := client.BulkIndex(
				context.Background(), protocolIndex, protocolDocuments(t, 1, 1),
			)
			if tc.want == nil {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				requireProtocolError(t, err, tc.want)
				if tc.want == ErrResponse {
					requireProtocolError(t, err, ErrStateUnknown)
				}
			}
		})
	}
}

func TestESProtocolBulkStopsAfterPartialFailure(t *testing.T) {
	documents := protocolDocuments(t, 1001, 1)
	first := protocolBulkItems(protocolIDs(documents[:500]))
	second := protocolBulkItems(protocolIDs(documents[500:1000]))
	failed := second[0].(map[string]any)["index"].(map[string]any)
	failed["status"] = 429
	failed["error"] = map[string]any{"reason": protocolSecret}

	client := protocolScript(t,
		protocolUnaliasedStep(),
		protocolStep{
			method: http.MethodPost,
			path:   "/" + protocolIndex + "/_bulk",
			body: protocolJSON(t, map[string]any{
				"errors": false, "items": first,
			}),
		},
		protocolStep{
			method: http.MethodPost,
			path:   "/" + protocolIndex + "/_bulk",
			body: protocolJSON(t, map[string]any{
				"errors": true, "items": second,
			}),
		},
	)

	err := client.BulkIndex(context.Background(), protocolIndex, documents)
	requireProtocolError(t, err, ErrRejected)
	// The script permits no third batch and no retry of the second batch.
}

func TestESProtocolBulkUncertainResponses(t *testing.T) {
	cases := []struct {
		name  string
		step  protocolStep
		cause error
	}{
		{"server_error", protocolStep{status: 503, body: protocolSecret}, nil},
		{"truncated_json", protocolStep{body: `{"errors":false,"items":[`}, ErrResponse},
		{"timeout", protocolStep{err: context.DeadlineExceeded}, context.DeadlineExceeded},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			step := tc.step
			step.method = http.MethodPost
			step.path = "/" + protocolIndex + "/_bulk"
			client := protocolScript(t, protocolUnaliasedStep(), step)

			err := client.BulkIndex(
				context.Background(), protocolIndex, protocolDocuments(t, 1, 1),
			)
			requireProtocolError(t, err, ErrStateUnknown)
			if tc.cause != nil {
				requireProtocolError(t, err, tc.cause)
			}
		})
	}
}
