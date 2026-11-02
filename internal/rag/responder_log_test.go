package rag_test

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"video-support-agent/internal/llm"
	"video-support-agent/internal/model"
	"video-support-agent/internal/rag"
	"video-support-agent/internal/retrieval"

	"github.com/cloudwego/eino/schema"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func requireResponderLog(
	t *testing.T,
	logs *observer.ObservedLogs,
	level zapcore.Level,
	expected map[string]any,
	retrieved, prepared, generated, historyEvaluated bool,
) observer.LoggedEntry {
	t.Helper()

	entries := logs.All()
	if len(entries) != 1 {
		t.Fatalf("log count=%d, want 1", len(entries))
	}
	entry := entries[0]
	if entry.Message != "rag_response" ||
		entry.LoggerName != "rag" ||
		entry.Level != level {
		t.Fatalf("unexpected log entry: %+v", entry.Entry)
	}

	fields := entry.ContextMap()
	allowed := map[string]bool{}
	for _, key := range []string{
		"outcome", "error_kind", "decision", "fixed_reason",
		"rag_duration_ms", "retrieval_called", "context_prepared",
		"generation_called", "generation_status", "citation_count",
	} {
		allowed[key] = true
	}
	if retrieved {
		allowed["retrieval_duration_ms"] = true
	}
	if generated {
		allowed["generation_duration_ms"] = true
	}
	if prepared {
		for _, key := range []string{
			"context_count", "rules_bytes", "current_message_bytes",
			"input_text_bytes", "evidence_input_count",
			"evidence_accepted_count", "invalid_metadata_count",
			"invalid_source_count", "inactive_count", "history_evaluated",
		} {
			allowed[key] = true
		}
	}
	if historyEvaluated {
		for _, key := range []string{
			"history_input_messages",
			"history_kept_messages",
			"history_dropped_messages",
		} {
			allowed[key] = true
		}
	}

	if len(fields) != len(allowed) || len(entry.Context) != len(allowed) {
		t.Fatalf("unexpected field count or duplicate fields: %+v", fields)
	}
	for key := range allowed {
		if _, ok := fields[key]; !ok {
			t.Errorf("missing field %q", key)
		}
	}
	for key := range fields {
		if !allowed[key] {
			t.Errorf("unexpected field %q", key)
		}
	}
	for key, want := range expected {
		if !reflect.DeepEqual(fields[key], want) {
			t.Errorf("%s=%v (%T), want %v (%T)",
				key, fields[key], fields[key], want, want)
		}
	}

	for key, want := range map[string]bool{
		"retrieval_called":  retrieved,
		"context_prepared":  prepared,
		"generation_called": generated,
	} {
		if fields[key] != want {
			t.Errorf("%s=%v, want %v", key, fields[key], want)
		}
	}
	if prepared && fields["history_evaluated"] != historyEvaluated {
		t.Errorf("history_evaluated=%v", fields["history_evaluated"])
	}

	for key := range allowed {
		if !strings.HasSuffix(key, "_duration_ms") {
			continue
		}
		value, ok := fields[key].(float64)
		if !ok || value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
			t.Errorf("invalid duration %s=%v", key, fields[key])
		}
	}
	return entry
}

func requireResponderLogPrivacy(
	t *testing.T,
	entry observer.LoggedEntry,
	markers ...string,
) {
	t.Helper()
	encoded, err := json.Marshal(struct {
		Message string         `json:"message"`
		Logger  string         `json:"logger"`
		Fields  map[string]any `json:"fields"`
	}{
		Message: entry.Message,
		Logger:  entry.LoggerName,
		Fields:  entry.ContextMap(),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range markers {
		if strings.Contains(string(encoded), marker) {
			t.Errorf("private marker leaked into log: %q", marker)
		}
	}
}

func TestRAGResponderLogOutcomes(t *testing.T) {
	cases := []struct {
		name      string
		outcome   string
		errorKind string
		decision  string
		reason    string
		status    string
		level     zapcore.Level
		wantErr   error
		retrieved bool
		prepared  bool
		generated bool
		history   bool
		citations int64
	}{
		{
			name: "answer", outcome: "generated", errorKind: "none",
			decision: "answer", reason: "none", status: "returned",
			level: zap.InfoLevel, retrieved: true, prepared: true,
			generated: true, history: true, citations: 1,
		},
		{
			name: "long_question", outcome: "fixed", errorKind: "none",
			decision: "clarify", reason: "question_too_long", status: "not_run",
			level: zap.InfoLevel,
		},
		{
			name: "no_evidence", outcome: "fixed", errorKind: "none",
			decision: "insufficient", reason: "no_evidence", status: "not_run",
			level: zap.InfoLevel, retrieved: true, prepared: true,
		},
		{
			name: "degraded_empty", outcome: "fixed", errorKind: "none",
			decision: "insufficient", reason: "degraded_empty", status: "not_run",
			level: zap.InfoLevel, retrieved: true, prepared: true,
		},
		{
			name: "retrieval_failure", outcome: "failed", errorKind: "retrieval_failed",
			decision: "none", reason: "none", status: "not_run",
			level: zap.WarnLevel, wantErr: rag.ErrRetrievalFailed, retrieved: true,
		},
		{
			name: "generation_failure", outcome: "failed", errorKind: "generation_failed",
			decision: "none", reason: "none", status: "failed",
			level: zap.WarnLevel, wantErr: rag.ErrGenerationFailed,
			retrieved: true, prepared: true, generated: true, history: true,
		},
		{
			name: "generation_timeout", outcome: "failed", errorKind: "generation_failed",
			decision: "none", reason: "none", status: "timeout",
			level: zap.WarnLevel, wantErr: rag.ErrGenerationFailed,
			retrieved: true, prepared: true, generated: true, history: true,
		},
		{
			name: "generation_canceled", outcome: "failed", errorKind: "generation_failed",
			decision: "none", reason: "none", status: "canceled",
			level: zap.WarnLevel, wantErr: rag.ErrGenerationFailed,
			retrieved: true, prepared: true, generated: true, history: true,
		},
		{
			name: "invalid_answer", outcome: "failed", errorKind: "invalid_answer",
			decision: "none", reason: "none", status: "returned",
			level: zap.WarnLevel, wantErr: rag.ErrInvalidAnswer,
			retrieved: true, prepared: true, generated: true, history: true,
		},
		{
			name: "invalid_input", outcome: "failed", errorKind: "invalid_input",
			decision: "none", reason: "none", status: "not_run",
			level: zap.InfoLevel, wantErr: llm.ErrInvalidInput,
		},
		{
			name: "parent_canceled", outcome: "failed", errorKind: "canceled",
			decision: "none", reason: "none", status: "not_run",
			level: zap.InfoLevel, wantErr: context.Canceled,
		},
		{
			name: "reserve_exhausted", outcome: "failed", errorKind: "request_timeout",
			decision: "none", reason: "none", status: "not_run",
			level: zap.WarnLevel, wantErr: context.DeadlineExceeded,
		},
		{
			name: "invalid_history", outcome: "failed", errorKind: "invalid_history",
			decision: "none", reason: "none", status: "not_run",
			level: zap.WarnLevel, wantErr: rag.ErrInvalidHistory, retrieved: true,
		},
		{
			name: "model_clarify", outcome: "generated", errorKind: "none",
			decision: "clarify", reason: "none", status: "returned",
			level: zap.InfoLevel, retrieved: true, prepared: true,
			generated: true, history: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			core, logs := observer.New(zap.DebugLevel)
			h := newResponderHarness(t)
			ctx := context.Background()
			question := "测试问题"
			var history []llm.HistoryMessage

			switch tc.name {
			case "long_question":
				question = strings.Repeat("问", 201)
			case "no_evidence":
				h.result = fixtureResult()
			case "degraded_empty":
				h.result = fixtureResult()
				h.result.Outcome = retrieval.HybridDegradedEmpty
				h.result.Degraded = true
			case "retrieval_failure":
				h.retrieveErr = errors.New("PRIVATE_RETRIEVAL_ERROR")
			case "generation_failure":
				h.generateErr = errors.New("PRIVATE_GENERATION_ERROR")
			case "generation_timeout":
				h.generateErr = context.DeadlineExceeded
			case "generation_canceled":
				h.generateErr = context.Canceled
			case "invalid_answer":
				h.raw.Content = "PRIVATE_INVALID_OUTPUT"
			case "invalid_input":
				question = " "
			case "parent_canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "reserve_exhausted":
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 3*time.Second)
				defer cancel()
			case "invalid_history":
				history = []llm.HistoryMessage{
					{Role: "system", Content: "PRIVATE_INVALID_HISTORY"},
				}
			case "model_clarify":
				h.raw.Content = outputJSON(t, "clarify")
			}

			r := h.responder(t, 2*time.Second, zap.New(core))
			reply, err := r.ReplyGrounded(ctx, history, question)
			if tc.wantErr != nil {
				requireAnswerError(t, reply, err, tc.wantErr)
			} else if err != nil {
				t.Fatal(err)
			}

			entry := requireResponderLog(t, logs, tc.level, map[string]any{
				"outcome":           tc.outcome,
				"error_kind":        tc.errorKind,
				"decision":          tc.decision,
				"fixed_reason":      tc.reason,
				"generation_status": tc.status,
				"citation_count":    tc.citations,
			}, tc.retrieved, tc.prepared, tc.generated, tc.history)

			requireResponderLogPrivacy(t, entry,
				"PRIVATE_RETRIEVAL_ERROR",
				"PRIVATE_GENERATION_ERROR",
				"PRIVATE_INVALID_OUTPUT",
				"PRIVATE_INVALID_HISTORY",
			)
		})
	}
}

func TestRAGResponderLogMetricsAndPrivacy(t *testing.T) {
	const (
		questionMarker = "PRIVATE_QUESTION"
		historyMarker  = "PRIVATE_HISTORY"
		articleMarker  = "PRIVATE_ARTICLE"
		titleMarker    = "PRIVATE_TITLE"
		sourceMarker   = "PRIVATE_SOURCE"
		outputMarker   = "PRIVATE_MODEL_STATEMENT"
		errorMarker    = "PRIVATE_UNDERLYING_ERROR"
	)

	t.Run("actual_message_bytes_and_citation_subset", func(t *testing.T) {
		first, second := fixtureItem(1), fixtureItem(2)
		for _, item := range []*retrieval.HybridItem{&first, &second} {
			item.Rule.Chunk.Title = titleMarker
			item.Rule.Chunk.SourceURL =
				"https://www.bilibili.com/__rag_fixture__/" + sourceMarker
			item.Rule.ArticleContent += articleMarker
		}

		h := newResponderHarness(t)
		h.result = fixtureResult(first, second)
		row := normalAnswerRow()
		row.Text = outputMarker
		row.Evidence[0].RefID = "R2"
		h.raw.Content = outputJSON(t, "answer", row)

		history := []llm.HistoryMessage{
			{Role: "assistant", Content: "PRIVATE_ORPHAN"},
			{Role: "user", Content: historyMarker},
			{Role: "assistant", Content: "PRIVATE_HISTORY_ANSWER"},
		}
		core, logs := observer.New(zap.DebugLevel)
		r := h.responder(t, 2*time.Second, zap.New(core))
		reply, err := r.ReplyGrounded(context.Background(), history, questionMarker)
		if err != nil ||
			reply.Decision != model.ReplyAnswer ||
			len(reply.Citations) != 1 ||
			reply.Citations[0].RefID != "R2" {
			t.Fatalf("reply=%+v error=%v", reply, err)
		}
		h.requireCalls(t, 1, 1)
		if len(h.messages) != 4 {
			t.Fatalf("message count=%d", len(h.messages))
		}

		current := h.messages[len(h.messages)-1].Content
		var payload struct {
			Question string          `json:"question"`
			Rules    json.RawMessage `json:"rules"`
		}
		if err := json.Unmarshal([]byte(current), &payload); err != nil {
			t.Fatal(err)
		}
		var rules []json.RawMessage
		if err := json.Unmarshal(payload.Rules, &rules); err != nil {
			t.Fatal(err)
		}
		if payload.Question != questionMarker || len(rules) != 2 {
			t.Fatal("unexpected generated input")
		}

		inputBytes := 0
		for _, message := range h.messages {
			inputBytes += len(message.Content)
		}
		entry := requireResponderLog(t, logs, zap.InfoLevel, map[string]any{
			"outcome":                  "generated",
			"error_kind":               "none",
			"decision":                 "answer",
			"fixed_reason":             "none",
			"generation_status":        "returned",
			"citation_count":           int64(1),
			"context_count":            int64(2),
			"rules_bytes":              int64(len(payload.Rules)),
			"current_message_bytes":    int64(len(current)),
			"input_text_bytes":         int64(inputBytes),
			"evidence_input_count":     int64(2),
			"evidence_accepted_count":  int64(2),
			"invalid_metadata_count":   int64(0),
			"invalid_source_count":     int64(0),
			"inactive_count":           int64(0),
			"history_input_messages":   int64(3),
			"history_kept_messages":    int64(2),
			"history_dropped_messages": int64(1),
		}, true, true, true, true)

		requireResponderLogPrivacy(t, entry,
			questionMarker, historyMarker, articleMarker,
			titleMarker, sourceMarker, outputMarker,
			"PRIVATE_ORPHAN", "PRIVATE_HISTORY_ANSWER",
		)
	})

	for _, stage := range []string{"retrieve", "generate", "invalid_output"} {
		t.Run("failure_privacy_"+stage, func(t *testing.T) {
			h := newResponderHarness(t)
			want := rag.ErrGenerationFailed
			prepared, generated := true, true
			status := "failed"
			kind := "generation_failed"
			switch stage {
			case "retrieve":
				h.retrieveErr = errors.New(errorMarker)
				want = rag.ErrRetrievalFailed
				prepared, generated = false, false
				status, kind = "not_run", "retrieval_failed"
			case "generate":
				h.generateErr = errors.New(errorMarker)
			case "invalid_output":
				h.raw = &schema.Message{
					Role:    schema.Assistant,
					Content: outputMarker,
					ResponseMeta: &schema.ResponseMeta{
						FinishReason: "stop",
					},
				}
				want = rag.ErrInvalidAnswer
				status, kind = "returned", "invalid_answer"
			}

			core, logs := observer.New(zap.DebugLevel)
			r := h.responder(t, 2*time.Second, zap.New(core))
			reply, err := r.ReplyGrounded(
				context.Background(), nil, questionMarker,
			)
			requireAnswerError(t, reply, err, want)
			if strings.Contains(err.Error(), errorMarker) ||
				strings.Contains(err.Error(), outputMarker) {
				t.Fatal("private data escaped through public error")
			}

			entry := requireResponderLog(t, logs, zap.WarnLevel, map[string]any{
				"outcome":           "failed",
				"error_kind":        kind,
				"decision":          "none",
				"fixed_reason":      "none",
				"generation_status": status,
				"citation_count":    int64(0),
			}, true, prepared, generated, prepared)

			requireResponderLogPrivacy(t, entry,
				questionMarker, errorMarker, outputMarker,
			)
		})
	}
}
