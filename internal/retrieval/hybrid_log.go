package retrieval

import (
	"context"
	"errors"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

type LoggedHybridRetriever struct {
	next   HybridRetriever
	logger *zap.Logger
}

var _ HybridRetriever = (*LoggedHybridRetriever)(nil)

func NewLoggedHybridRetriever(next HybridRetriever, logger *zap.Logger) (*LoggedHybridRetriever, error) {
	if next == nil || logger == nil {
		return nil, ErrHybridConfiguration
	}

	return &LoggedHybridRetriever{
		next:   next,
		logger: logger.Named("hybrid"),
	}, nil
}

func (r *LoggedHybridRetriever) Retrieve(ctx context.Context, query string, opts HybridOptions) (HybridResult, error) {
	if r == nil || r.next == nil || r.logger == nil {
		return HybridResult{
			Outcome:   HybridFailed,
			ErrorKind: "configuration",
		}, ErrHybridConfiguration
	}

	result, err := r.next.Retrieve(ctx, query, opts)

	level := zapcore.InfoLevel
	if err != nil {
		if !errors.Is(err, context.Canceled) &&
			!errors.Is(err, ErrInvalidQuery) {
			level = zapcore.WarnLevel
		}
	} else if result.Degraded ||
		result.Stats.Rerank.Status == HybridStageFallback {
		level = zapcore.WarnLevel
	}

	r.logger.Log(level, "hybrid_retrieval", hybridLogFields(result, err)...)

	return result, err
}

func hybridLogFields(result HybridResult, err error) []zap.Field {
	outcome := safeHybridLogLabel(
		string(result.Outcome),
		string(HybridSuccess),
		string(HybridDegraded),
		string(HybridNoEvidence),
		string(HybridDegradedEmpty),
		string(HybridNoUsableEvidence),
		string(HybridFailed),
	)
	if err != nil {
		outcome = string(HybridFailed)
	}

	fields := []zap.Field{
		zap.String("outcome", outcome),
		zap.Bool("has_error", err != nil),
		zap.String("error_kind", classifyHybridError(err)),
		zap.Bool("degraded", result.Degraded),
		zap.String(
			"empty_reason",
			safeHybridLogLabel(
				result.EmptyReason,
				"",
				"no_candidates",
				"conflicts",
				"current_filter",
			),
		),
		zap.Float64("retrieval_duration_ms", float64(result.Duration)/float64(time.Millisecond)),
		zap.Float64("source_duration_ms", float64(result.Stats.SourceDuration)/float64(time.Millisecond)),
		zap.Int("final_count", result.Stats.FinalCount),
	}

	fields = append(
		fields,
		hybridSourceLogFields("es", result.Stats.Sources[0])...,
	)
	fields = append(
		fields,
		hybridSourceLogFields("milvus", result.Stats.Sources[1])...,
	)

	stages := []struct {
		prefix string
		report HybridStageReport
	}{
		{"fusion", result.Stats.Fusion},
		{"hydration", result.Stats.Hydration},
		{"rerank", result.Stats.Rerank},
	}

	for _, stage := range stages {
		fields = append(fields, hybridStageLogFields(stage.prefix, stage.report)...)
	}

	fusionKnown := result.Stats.Fusion.Status == HybridStageSuccess
	hydrationKnown := result.Stats.Hydration.Status == HybridStageSuccess

	fields = append(fields, zap.Bool("fusion_counts_known", fusionKnown), zap.Bool("hydration_counts_known", hydrationKnown))

	if fusionKnown {
		fields = append(
			fields,
			zap.Int("raw_count", result.Stats.RawCount),
			zap.Int("duplicate_count", result.Stats.DuplicateCount),
			zap.Int("conflict_count", result.Stats.ConflictCount),
			zap.Int("unique_count", result.Stats.UniqueCount),
			zap.Int("clipped_count", result.Stats.ClippedCount),
			zap.Int("fused_count", result.Stats.FusedCount),
		)
	}

	if hydrationKnown {
		fields = append(
			fields,
			zap.Int("hydrated_count", result.Stats.HydratedCount),
			zap.Int("filtered_count", result.Stats.FilteredCount),
			zap.Int("article_count", result.Stats.ArticleCount),
		)
	}

	return fields
}

func hybridSourceLogFields(prefix string, report SourceReport) []zap.Field {
	fields := []zap.Field{
		zap.String(
			prefix+"_status",
			safeHybridLogLabel(
				string(report.Status),
				string(SourceNotStarted),
				string(SourceRunning),
				string(SourceSucceeded),
				string(SourceEmpty),
				string(SourceFailed),
			),
		),
		zap.String(prefix+"_error_kind", safeHybridLogErrorKind(string(report.ErrorKind))),
		zap.Float64(prefix+"_duration_ms", float64(report.Duration)/float64(time.Millisecond)),
		zap.Bool(prefix+"_raw_count_known", report.RawCountKnown),
	}

	if report.RawCountKnown {
		fields = append(fields, zap.Int(prefix+"_raw_count", report.RawCount))
	}

	return fields
}

func hybridStageLogFields(prefix string, report HybridStageReport) []zap.Field {
	return []zap.Field{
		zap.String(
			prefix+"_status",
			safeHybridLogLabel(
				string(report.Status),
				string(HybridStageNotRun),
				string(HybridStageSkipped),
				string(HybridStageSuccess),
				string(HybridStageFallback),
				string(HybridStageFailed),
			),
		),
		zap.String(prefix+"_error_kind", safeHybridLogErrorKind(report.ErrorKind)),
		zap.Float64(prefix+"_duration_ms", float64(report.Duration)/float64(time.Millisecond)),
	}
}

func safeHybridLogErrorKind(value string) string {
	return safeHybridLogLabel(
		value,
		"",
		"canceled",
		"timeout",
		"configuration",
		"invalid_query",
		"unavailable",
		"hydration_limit",
		"invalid_rerank",
		"hydration",
		"invalid_candidates",
		"index_not_ready",
		"dependency",
	)
}

func safeHybridLogLabel(value string, allowed ...string) string {
	for _, label := range allowed {
		if value == label {
			return label
		}
	}
	return "unknown"
}
