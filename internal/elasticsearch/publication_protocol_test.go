package elasticsearch

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	indexing "video-support-agent/internal/index"
)

func protocolVerificationSteps(
	t *testing.T,
	documents []indexing.EncodedDocument,
) []protocolStep {
	t.Helper()

	shards := map[string]any{"total": 1, "successful": 1, "failed": 0}
	steps := []protocolStep{
		protocolUnaliasedStep(),
		{
			method: http.MethodPost,
			path:   "/" + protocolIndex + "/_refresh",
			body:   protocolJSON(t, map[string]any{"_shards": shards}),
		},
		{
			method: http.MethodGet,
			path:   "/" + protocolIndex + "/_count",
			body: protocolJSON(t, map[string]any{
				"count": len(documents), "_shards": shards,
			}),
		},
	}
	if len(documents) == 0 {
		return steps
	}

	seen := make(map[int]bool)
	for _, position := range []int{0, len(documents) / 2, len(documents) - 1} {
		if seen[position] {
			continue
		}
		seen[position] = true
		document := documents[position]
		steps = append(steps, protocolStep{
			method: http.MethodGet,
			path: "/" + protocolIndex + "/_doc/" +
				document.ID + "?realtime=false",
			body: protocolJSON(t, map[string]any{
				"_index":  protocolIndex,
				"_id":     document.ID,
				"found":   true,
				"_source": json.RawMessage(document.JSON),
			}),
		})
	}
	return steps
}

func TestESProtocolVerificationSuccess(t *testing.T) {
	for _, count := range []int{0, 1, 2, 5} {
		documents := protocolDocuments(t, count, 10)
		client := protocolScript(t, protocolVerificationSteps(t, documents)...)
		if err := client.VerifyIndex(
			context.Background(), protocolIndex, documents,
		); err != nil {
			t.Fatalf("count=%d: %v", count, err)
		}
	}
}

func TestESProtocolVerificationFailures(t *testing.T) {
	cases := []struct {
		name   string
		at     int
		status int
		body   string
		want   error
	}{
		{"refresh_status", 1, 503, protocolSecret, ErrRejected},
		{"refresh_failed_shard", 1, 200,
			`{"_shards":{"total":1,"successful":0,"failed":1}}`, ErrResponse},
		{"refresh_missing_shards", 1, 200, `{}`, ErrResponse},
		{"count_status", 2, 503, protocolSecret, ErrRejected},
		{"count_mismatch", 2, 200,
			`{"count":4,"_shards":{"total":1,"successful":1,"failed":0}}`,
			ErrRejected},
		{"count_partial", 2, 200,
			`{"count":5,"_shards":{"total":2,"successful":1,"failed":1}}`,
			ErrResponse},
		{"count_missing", 2, 200,
			`{"_shards":{"total":1,"successful":1,"failed":0}}`, ErrResponse},
		{"sample_missing", 3, 404, `{}`, ErrRejected},
		{"sample_wrong_id", 3, 200,
			`{"_index":"test_new_index","_id":"99","found":true,"_source":{}}`,
			ErrResponse},
		{"sample_wrong_index", 3, 200,
			`{"_index":"other","_id":"1","found":true,"_source":{}}`,
			ErrResponse},
		{"sample_not_found", 3, 200,
			`{"_index":"test_new_index","_id":"1","found":false}`,
			ErrResponse},
		{"sample_changed", 3, 200,
			`{"_index":"test_new_index","_id":"1","found":true,"_source":{"chunk_id":"1","content":"changed"}}`,
			ErrRejected},
		{"sample_null", 3, 200,
			`{"_index":"test_new_index","_id":"1","found":true,"_source":null}`,
			ErrResponse},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			documents := protocolDocuments(t, 5, 10)
			steps := protocolVerificationSteps(t, documents)
			steps[tc.at].status = tc.status
			steps[tc.at].body = tc.body
			steps = steps[:tc.at+1]

			client := protocolScript(t, steps...)
			requireProtocolError(t,
				client.VerifyIndex(context.Background(), protocolIndex, documents),
				tc.want)
		})
	}
}

func checkProtocolAliasActions(t *testing.T, old string) func(*http.Request) {
	t.Helper()

	return func(r *http.Request) {
		if r.Header.Get("Content-Type") != "application/json" {
			t.Fatal("alias action Content-Type missing")
		}
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		var body struct {
			Actions []map[string]map[string]any `json:"actions"`
		}
		if err := json.Unmarshal(data, &body); err != nil {
			t.Fatal(err)
		}

		addPosition := 0
		if old != "" {
			addPosition = 1
		}
		if len(body.Actions) != addPosition+1 {
			t.Fatal("remove and add were not sent in one request")
		}

		if old != "" {
			action := body.Actions[0]
			remove := action["remove"]
			if len(action) != 1 ||
				remove["index"] != old ||
				remove["alias"] != protocolAlias ||
				remove["must_exist"] != true {
				t.Fatal("remove action lacks expected target or must_exist")
			}
		}

		action := body.Actions[addPosition]
		add := action["add"]
		writeFlag, exists := add["is_write_index"]
		if len(action) != 1 ||
			add["index"] != protocolIndex ||
			add["alias"] != protocolAlias ||
			!exists || writeFlag != false {
			t.Fatal("add action lacks target or explicit write prohibition")
		}
	}
}

func TestESProtocolAliasPublication(t *testing.T) {
	cases := []struct {
		name         string
		mode         string
		observed     string
		wantState    indexing.PublicationState
		wantError    error
		acknowledged bool
	}{
		{"normal", "normal", protocolIndex,
			indexing.PublicationApplied, nil, true},
		{"timeout_new", "timeout", protocolIndex,
			indexing.PublicationApplied, context.DeadlineExceeded, false},
		{"timeout_old", "timeout", protocolOld,
			indexing.PublicationOldObserved, context.DeadlineExceeded, false},
		{"timeout_other", "timeout", "unexpected_index",
			indexing.PublicationUnknown, context.DeadlineExceeded, false},
		{"cancel_new", "cancel", protocolIndex,
			indexing.PublicationApplied, context.Canceled, false},
		{"bad_ack_new", "bad_ack", protocolIndex,
			indexing.PublicationApplied, ErrResponse, false},
		{"unacknowledged_new", "unacknowledged", protocolIndex,
			indexing.PublicationApplied, ErrStateUnknown, false},
		{"server_error_old", "server_error", protocolOld,
			indexing.PublicationOldObserved, ErrStateUnknown, false},
		{"read_failure", "timeout_read_failure", "",
			indexing.PublicationUnknown, ErrStateUnknown, false},
		{"ack_but_old", "normal", protocolOld,
			indexing.PublicationOldObserved, ErrStateUnknown, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			publish := protocolStep{
				method: http.MethodPost,
				path:   "/_aliases",
				body:   `{"acknowledged":true}`,
				check:  checkProtocolAliasActions(t, protocolOld),
			}
			switch tc.mode {
			case "timeout", "timeout_read_failure":
				publish.err = context.DeadlineExceeded
			case "cancel":
				check := publish.check
				publish.check = func(r *http.Request) {
					check(r)
					cancel()
				}
				publish.err = context.Canceled
			case "bad_ack":
				publish.body = `{`
			case "unacknowledged":
				publish.body = `{"acknowledged":false}`
			case "server_error":
				publish.status = 503
				publish.body = protocolSecret
			}

			observe := protocolStep{
				method: http.MethodGet,
				path:   "/_alias/" + protocolAlias,
				body:   protocolAliasBody(tc.observed),
				check: func(r *http.Request) {
					if tc.mode == "normal" {
						return
					}
					if r.Context().Err() != nil {
						t.Fatal("recovery reused a canceled context")
					}
					deadline, exists := r.Context().Deadline()
					if !exists || time.Until(deadline) <= 0 ||
						time.Until(deadline) > aliasRecoveryTimeout {
						t.Fatal("recovery read is not bounded")
					}
				},
			}
			if tc.mode == "timeout_read_failure" {
				observe.status = 503
				observe.body = protocolSecret
			}

			client := protocolScript(t,
				protocolStep{
					method: http.MethodGet,
					path:   "/_alias/" + protocolAlias,
					body:   protocolAliasBody(protocolOld),
				},
				protocolUnaliasedStep(),
				publish,
				observe,
			)

			result, err := client.SwitchAlias(ctx, protocolOld, protocolIndex)
			if result.State != tc.wantState ||
				result.ObservedTarget != tc.observed ||
				result.RequestAcknowledged != tc.acknowledged {
				t.Fatalf("unexpected publication observation: %+v", result)
			}
			if tc.wantError != nil {
				requireProtocolError(t, err, tc.wantError, ErrStateUnknown)
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestESProtocolAliasPublicationGuards(t *testing.T) {
	t.Run("changed_old_target", func(t *testing.T) {
		client := protocolScript(t, protocolStep{
			method: http.MethodGet,
			path:   "/_alias/" + protocolAlias,
			body:   protocolAliasBody("another_current"),
		})
		result, err := client.SwitchAlias(
			context.Background(), protocolOld, protocolIndex,
		)
		requireProtocolError(t, err, ErrRejected)
		if result.State != indexing.PublicationNotAttempted {
			t.Fatal("changed target reached publication")
		}
	})

	t.Run("new_target_already_aliased", func(t *testing.T) {
		client := protocolScript(t,
			protocolStep{
				method: http.MethodGet,
				path:   "/_alias/" + protocolAlias,
				body:   protocolAliasBody(protocolOld),
			},
			protocolStep{
				method: http.MethodGet,
				path:   "/" + protocolIndex + "/_alias",
				body:   protocolAliasBody(protocolIndex),
			},
		)
		result, err := client.SwitchAlias(
			context.Background(), protocolOld, protocolIndex,
		)
		requireProtocolError(t, err, ErrRejected)
		if result.State != indexing.PublicationNotAttempted {
			t.Fatal("already published new target was reused")
		}
	})

	t.Run("missing_old_alias", func(t *testing.T) {
		client := protocolScript(t,
			protocolStep{
				method: http.MethodGet,
				path:   "/_alias/" + protocolAlias,
				status: 404,
				body:   `{"error":"alias missing","status":404}`,
			},
			protocolUnaliasedStep(),
			protocolStep{
				method: http.MethodPost,
				path:   "/_aliases",
				body:   `{"acknowledged":true}`,
				check:  checkProtocolAliasActions(t, ""),
			},
			protocolStep{
				method: http.MethodGet,
				path:   "/_alias/" + protocolAlias,
				body:   protocolAliasBody(protocolIndex),
			},
		)
		result, err := client.SwitchAlias(
			context.Background(), "", protocolIndex,
		)
		if err != nil || result.State != indexing.PublicationApplied {
			t.Fatalf("initial publication failed: %+v, %v", result, err)
		}
	})

	t.Run("invalid_or_canceled_before_request", func(t *testing.T) {
		client := protocolScript(t)
		result, err := client.SwitchAlias(
			context.Background(), protocolOld, protocolOld,
		)
		requireProtocolError(t, err, ErrRejected)
		if result.State != indexing.PublicationNotAttempted {
			t.Fatal("invalid target reached publication")
		}

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		result, err = client.SwitchAlias(ctx, protocolOld, protocolIndex)
		requireProtocolError(t, err, context.Canceled)
		if result.State != indexing.PublicationNotAttempted {
			t.Fatal("canceled operation reached publication")
		}
	})
}
