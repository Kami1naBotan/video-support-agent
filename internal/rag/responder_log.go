package rag

import (
	"context"
	"errors"
	"time"

	"video-support-agent/internal/llm"
	"video-support-agent/internal/model"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

type responseReport struct {
	retrievalCalled   bool
	retrievalDuration time.Duration

	contextPrepared bool
	contextStats    ContextStats
	fixedReason     FixedReason

	generationCalled   bool
	generationStatus   string
	generationDuration time.Duration
}

func (r *Responder) logResponse(started time.Time, report responseReport, reply model.GroundedReply, err error) {
	outcome := "generated"
	if err != nil {
		outcome = "failed"
	} else if report.fixedReason != FixedNone {
		outcome = "fixed"
	}

	decision := "none"
	if err == nil {
		decision = responseDecisionLabel(reply.Decision)
	}

	fields := []zap.Field{
		zap.String("outcome", outcome),
		zap.String("error_kind", responseErrorKind(err)),
		zap.String("decision", decision),
		zap.String("fixed_reason", responseFixedReasonLabel(report.fixedReason)),
		zap.Float64("rag_duration_ms", responseMilliseconds(time.Since(started))),
		zap.Bool("retrieval_called", report.retrievalCalled),
		zap.Bool("context_prepared", report.contextPrepared),
		zap.Bool("generation_called", report.generationCalled),
		zap.String("generation_status", report.generationStatus),
		zap.Int("citation_count", len(reply.Citations)),
	}

	if report.retrievalCalled {
		fields = append(fields, zap.Float64("retrieval_duration_ms", responseMilliseconds(report.retrievalDuration)))
	}

	if report.generationCalled {
		fields = append(fields, zap.Float64("generation_duration_ms", responseMilliseconds(report.generationDuration)))
	}

	if report.contextPrepared {
		stats := report.contextStats

		fields = append(fields,
			zap.Int("context_count", stats.ContextCount),
			zap.Int("rules_bytes", stats.RulesBytes),
			zap.Int("current_message_bytes", stats.CurrentMessageBytes),
			zap.Int("input_text_bytes", stats.InputTextBytes),
			zap.Int("evidence_input_count", stats.Evidence.InputCount),
			zap.Int("evidence_accepted_count", stats.Evidence.AcceptedCount),
			zap.Int("invalid_metadata_count", stats.Evidence.InvalidMetadataCount),
			zap.Int("invalid_source_count", stats.Evidence.InvalidSourceCount),
			zap.Int("inactive_count", stats.Evidence.InactiveCount),
			zap.Bool("history_evaluated", stats.HistoryEvaluated),
		)

		if stats.HistoryEvaluated {
			fields = append(fields,
				zap.Int("history_input_messages", stats.HistoryInputMessages),
				zap.Int("history_kept_messages", stats.HistoryKeptMessages),
				zap.Int("history_dropped_messages", stats.HistoryDroppedMessages),
			)
		}
	}

	level := zapcore.InfoLevel
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, llm.ErrInvalidInput) {
		level = zapcore.WarnLevel
	}

	r.logger.Log(level, "rag_response", fields...)
}

func responseMilliseconds(duration time.Duration) float64 {
	return float64(duration) / float64(time.Millisecond)
}

func responseErrorKind(err error) string {
	switch {
	case err == nil:
		return "none"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "request_timeout"
	case errors.Is(err, llm.ErrInvalidInput):
		return "invalid_input"
	case errors.Is(err, ErrResponderConfiguration):
		return "configuration"
	case errors.Is(err, ErrRetrievalFailed):
		return "retrieval_failed"
	case errors.Is(err, ErrGenerationFailed):
		return "generation_failed"
	case errors.Is(err, ErrInvalidAnswer):
		return "invalid_answer"
	case errors.Is(err, ErrInvalidHistory):
		return "invalid_history"
	default:
		return "preparation_failed"
	}
}

func responseDecisionLabel(decision model.ReplyDecision) string {
	switch decision {
	case model.ReplyAnswer:
		return "answer"
	case model.ReplyClarify:
		return "clarify"
	case model.ReplyHandoff:
		return "handoff"
	case model.ReplyInsufficient:
		return "insufficient"
	default:
		return "unknown"
	}
}

func responseFixedReasonLabel(reason FixedReason) string {
	switch reason {
	case FixedNone:
		return "none"
	case FixedQuestionTooLong:
		return "question_too_long"
	case FixedContextTooLarge:
		return "context_too_large"
	case FixedNoEvidence:
		return "no_evidence"
	case FixedDegradedEmpty:
		return "degraded_empty"
	case FixedUnusableEvidence:
		return "unusable_evidence"
	default:
		return "unknown"
	}
}
