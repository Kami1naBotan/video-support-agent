package retrieval_test

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"video-support-agent/internal/retrieval"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

type ragRetrieveFunc func(
	context.Context, string, retrieval.HybridOptions,
) (retrieval.HybridResult, error)

func (f ragRetrieveFunc) Retrieve(
	ctx context.Context, query string, opts retrieval.HybridOptions,
) (retrieval.HybridResult, error) {
	return f(ctx, query, opts)
}

type ragPrivateError struct {
	calls atomic.Int32
}

func (e *ragPrivateError) Error() string {
	e.calls.Add(1)
	return "private-error-content"
}

func ragLogOnce(
	t *testing.T,
	result retrieval.HybridResult,
	returnedErr error,
) (observer.LoggedEntry, string) {
	t.Helper()
	core, observed := observer.New(zap.DebugLevel)
	var output bytes.Buffer
	jsonCore := zapcore.NewCore(
		zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()),
		zapcore.AddSync(&output),
		zap.DebugLevel,
	)
	logger := zap.New(zapcore.NewTee(core, jsonCore))
	next := ragRetrieveFunc(func(
		context.Context, string, retrieval.HybridOptions,
	) (retrieval.HybridResult, error) {
		return result, returnedErr
	})
	wrapped, err := retrieval.NewLoggedHybridRetriever(next, logger)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = wrapped.Retrieve(
		context.Background(), "private-query-content",
		retrieval.HybridOptions{VersionID: 987},
	)
	entries := observed.All()
	if len(entries) != 1 {
		t.Fatalf("log count=%d", len(entries))
	}
	if entries[0].Message != "hybrid_retrieval" ||
		entries[0].LoggerName != "hybrid" {
		t.Fatal("event name or logger name changed")
	}
	return entries[0], output.String()
}

func TestRAGLog_ForwardAndLevels(t *testing.T) {
	for _, mode := range []string{
		"success", "no_evidence", "degraded", "fallback",
		"invalid_query", "canceled", "deadline", "dependency",
	} {
		t.Run(mode, func(t *testing.T) {
			want := retrieval.HybridResult{
				Items: ragRerankItems(1), Outcome: retrieval.HybridSuccess,
			}
			var wantErr error
			wantLevel := zapcore.InfoLevel
			switch mode {
			case "no_evidence":
				want.Items = []retrieval.HybridItem{}
				want.Outcome = retrieval.HybridNoEvidence
			case "degraded":
				want.Degraded = true
				want.Outcome = retrieval.HybridDegraded
				wantLevel = zapcore.WarnLevel
			case "fallback":
				want.Stats.Rerank.Status = retrieval.HybridStageFallback
				wantLevel = zapcore.WarnLevel
			case "invalid_query":
				wantErr = retrieval.ErrInvalidQuery
			case "canceled":
				wantErr = context.Canceled
			case "deadline":
				wantErr = context.DeadlineExceeded
				wantLevel = zapcore.WarnLevel
			case "dependency":
				wantErr = errors.New("synthetic dependency failure")
				wantLevel = zapcore.WarnLevel
			}
			if wantErr != nil {
				want.Items = nil
				want.Outcome = retrieval.HybridFailed
			}

			core, logs := observer.New(zap.DebugLevel)
			ctx := context.WithValue(context.Background(), struct{}{}, "marker")
			opts := retrieval.HybridOptions{VersionID: 21, TopK: 3}
			calls := 0
			next := ragRetrieveFunc(func(
				gotCtx context.Context, query string,
				gotOpts retrieval.HybridOptions,
			) (retrieval.HybridResult, error) {
				calls++
				if gotCtx != ctx || query != "  原样问题  " || gotOpts != opts {
					t.Error("wrapper modified forwarded arguments")
				}
				return want, wantErr
			})
			wrapped, err := retrieval.NewLoggedHybridRetriever(next, zap.New(core))
			if err != nil {
				t.Fatal(err)
			}
			got, gotErr := wrapped.Retrieve(ctx, "  原样问题  ", opts)
			if calls != 1 || gotErr != wantErr || !reflect.DeepEqual(got, want) {
				t.Fatal("wrapper changed result/error or repeated the call")
			}
			entries := logs.All()
			if len(entries) != 1 || entries[0].Level != wantLevel {
				t.Fatalf("logs=%v expected level=%v", entries, wantLevel)
			}
		})
	}
}

func TestRAGLog_FieldsAndPrivacy(t *testing.T) {
	t.Run("known_counts_and_fractional_milliseconds", func(t *testing.T) {
		result := retrieval.HybridResult{
			Items:    ragRerankItems(1),
			Outcome:  retrieval.HybridSuccess,
			Duration: 1250 * time.Microsecond,
			Stats: retrieval.HybridStats{
				Sources: [2]retrieval.SourceReport{
					{
						Source:        retrieval.SourceES,
						Status:        retrieval.SourceSucceeded,
						Duration:      750 * time.Microsecond,
						RawCountKnown: true, RawCount: 2,
					},
					{
						Source:        retrieval.SourceMilvus,
						Status:        retrieval.SourceEmpty,
						RawCountKnown: true, RawCount: 0,
					},
				},
				SourceDuration: 750 * time.Microsecond,
				Fusion: retrieval.HybridStageReport{
					Status: retrieval.HybridStageSuccess,
				},
				Hydration: retrieval.HybridStageReport{
					Status: retrieval.HybridStageSuccess,
				},
				Rerank: retrieval.HybridStageReport{
					Status: retrieval.HybridStageSkipped,
				},
				RawCount: 2, UniqueCount: 2, FusedCount: 2,
				HydratedCount: 1, FilteredCount: 1,
				ArticleCount: 1, FinalCount: 1,
			},
		}
		entry, _ := ragLogOnce(t, result, nil)
		fields := entry.ContextMap()
		expected := map[string]any{
			"outcome":                "ok",
			"has_error":              false,
			"error_kind":             "",
			"retrieval_duration_ms":  float64(1.25),
			"source_duration_ms":     float64(0.75),
			"es_duration_ms":         float64(0.75),
			"es_raw_count_known":     true,
			"es_raw_count":           int64(2),
			"milvus_raw_count":       int64(0),
			"fusion_counts_known":    true,
			"hydration_counts_known": true,
			"raw_count":              int64(2),
			"duplicate_count":        int64(0),
			"conflict_count":         int64(0),
			"unique_count":           int64(2),
			"clipped_count":          int64(0),
			"fused_count":            int64(2),
			"hydrated_count":         int64(1),
			"filtered_count":         int64(1),
			"article_count":          int64(1),
			"final_count":            int64(1),
		}
		for key, want := range expected {
			if !reflect.DeepEqual(fields[key], want) {
				t.Errorf("%s=%v want=%v", key, fields[key], want)
			}
		}
	})

	for _, withError := range []bool{false, true} {
		name := "unknown_labels"
		if withError {
			name = "private_error"
		}
		t.Run(name, func(t *testing.T) {
			result := retrieval.HybridResult{
				Items:       ragRerankItems(1),
				Outcome:     "private-outcome",
				ErrorKind:   "private-result-error",
				EmptyReason: "private-empty-reason",
			}
			result.Items[0].Rule.Chunk.Title = "private-title"
			result.Items[0].Rule.Chunk.Content = "private-chunk"
			result.Items[0].Rule.ArticleContent = "private-article"
			result.Items[0].Rule.Chunk.SourceURL = "https://private-url.invalid"
			for i := range result.Stats.Sources {
				result.Stats.Sources[i] = retrieval.SourceReport{
					Source: "private-source", Status: "private-status",
					ErrorKind: "private-source-error",
					RawCount:  99, RawCountKnown: false,
				}
			}
			unknownStage := retrieval.HybridStageReport{
				Status: "private-stage", ErrorKind: "private-stage-error",
			}
			result.Stats.Fusion = unknownStage
			result.Stats.Hydration = unknownStage
			result.Stats.Rerank = unknownStage

			privateErr := &ragPrivateError{}
			var returnedErr error
			if withError {
				returnedErr = privateErr
			}
			entry, encoded := ragLogOnce(t, result, returnedErr)
			fields := entry.ContextMap()

			if strings.Contains(encoded, "private-") ||
				privateErr.calls.Load() != 0 {
				t.Fatal("log leaked private content or formatted the error")
			}
			for _, key := range []string{
				"empty_reason",
				"es_status", "es_error_kind",
				"milvus_status", "milvus_error_kind",
				"fusion_status", "fusion_error_kind",
				"hydration_status", "hydration_error_kind",
				"rerank_status", "rerank_error_kind",
			} {
				if fields[key] != "unknown" {
					t.Errorf("%s was not mapped to unknown", key)
				}
			}
			for _, key := range []string{
				"es_raw_count", "milvus_raw_count",
				"raw_count", "duplicate_count", "conflict_count",
				"unique_count", "clipped_count", "fused_count",
				"hydrated_count", "filtered_count", "article_count",
				"query", "options", "items", "error",
			} {
				if _, exists := fields[key]; exists {
					t.Errorf("unexpected field: %s", key)
				}
			}
			if fields["fusion_counts_known"] != false ||
				fields["hydration_counts_known"] != false {
				t.Fatal("unknown counts were presented as known")
			}
			wantOutcome, wantKind := "unknown", ""
			if withError {
				wantOutcome, wantKind = "failed", "dependency"
			}
			if fields["outcome"] != wantOutcome ||
				fields["error_kind"] != wantKind ||
				fields["has_error"] != withError {
				t.Fatal("overall log classification differs")
			}
		})
	}
}

func TestRAGLog_Construction(t *testing.T) {
	next := ragRetrieveFunc(func(
		context.Context, string, retrieval.HybridOptions,
	) (retrieval.HybridResult, error) {
		t.Fatal("constructor invoked the retriever")
		return retrieval.HybridResult{}, nil
	})
	if got, err := retrieval.NewLoggedHybridRetriever(nil, zap.NewNop()); got != nil || !errors.Is(err, retrieval.ErrHybridConfiguration) {
		t.Fatal("nil retriever accepted")
	}
	if got, err := retrieval.NewLoggedHybridRetriever(next, nil); got != nil || !errors.Is(err, retrieval.ErrHybridConfiguration) {
		t.Fatal("nil logger accepted")
	}
	var missing *retrieval.LoggedHybridRetriever
	_, err := missing.Retrieve(
		context.Background(), "测试", retrieval.HybridOptions{},
	)
	if !errors.Is(err, retrieval.ErrHybridConfiguration) {
		t.Fatal("nil wrapper did not return configuration error")
	}
}
