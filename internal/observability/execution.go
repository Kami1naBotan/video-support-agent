package observability

import (
	"context"
	"errors"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

type ExecutionResult struct {
	Outcome        string
	Reason         string
	BusinessEffect string
	AuditPending   bool
}

type ExecutionObserver struct {
	tracer        trace.Tracer
	toolCalls     *prometheus.CounterVec
	agentFailures *prometheus.CounterVec
	cancellations *prometheus.CounterVec
}

func NewExecutionObserver(runtime *Runtime) (*ExecutionObserver, error) {
	if runtime == nil {
		return nil, errors.New("execution observation runtime is required")
	}
	if !runtime.Enabled {
		return nil, nil
	}
	if runtime.Registry == nil || runtime.Provider == nil {
		return nil, errors.New("execution observation dependencies are required")
	}

	toolCalls := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "tool_calls_total",
			Help: "Actual tool invocations classified at completion.",
		},
		[]string{"tool", "phase", "outcome"},
	)
	failures := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "agent_failures_total",
			Help: "Failed, timed out or unknown Agent executions.",
		},
		[]string{"phase", "reason"},
	)
	cancellations := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "agent_cancellations_total",
			Help: "Cancelled Agent executions, separate from failures.",
		},
		[]string{"phase"},
	)

	collectors := []prometheus.Collector{
		toolCalls, failures, cancellations,
	}
	for i, collector := range collectors {
		if err := runtime.Registry.Register(collector); err != nil {
			for _, registered := range collectors[:i] {
				runtime.Registry.Unregister(registered)
			}
			return nil, errors.New("register execution metrics failed")
		}
	}

	return &ExecutionObserver{
		tracer:        runtime.Provider.Tracer("video-support-agent/execution"),
		toolCalls:     toolCalls,
		agentFailures: failures,
		cancellations: cancellations,
	}, nil
}

func (o *ExecutionObserver) Start(ctx context.Context, operation, phase string) (context.Context, func(ExecutionResult)) {
	if o == nil {
		return ctx, func(ExecutionResult) {}
	}

	operation = executionValue(operation, "",
		"agent",
		"search_rule",
		"get_rule_detail",
		"check_appeal_eligibility",
		"create_appeal_ticket",
		"query_ticket_status",
	)
	if operation == "" {
		return ctx, func(ExecutionResult) {}
	}
	phase = executionValue(phase, "other", "chat", "confirm")

	name := "agent.run"
	if operation != "agent" {
		name = "tool." + operation
	}
	ctx, span := o.tracer.Start(
		ctx,
		name,
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(attribute.String("app.phase", phase)),
	)

	var once sync.Once
	return ctx, func(result ExecutionResult) {
		once.Do(func() {
			defer span.End()

			outcome := executionValue(result.Outcome, "failed", "completed", "success", "rejected", "failed", "timed_out", "cancelled", "unknown")
			reason := executionValue(result.Reason, "other",
				"none", "panic", "timed_out", "cancelled",
				"outcome_unknown", "invalid_argument",
				"unauthorized", "not_found", "configuration_error",
				"audit_unavailable", "unavailable", "invalid_result",
				"result_too_large", "event_failed", "protocol_error",
				"budget_exhausted", "confirmation_required",
				"precondition_changed", "needs_more_info",
				"ineligible", "idempotency_conflict",
				"create_already_attempted", "no_evidence",
			)
			effect := executionValue(result.BusinessEffect, "none", "none", "committed", "unknown")

			span.SetAttributes(
				attribute.String("app.outcome", outcome),
				attribute.String("app.reason", reason),
				attribute.String("app.business_effect", effect),
				attribute.Bool("app.audit_pending", result.AuditPending),
			)

			switch outcome {
			case "failed", "timed_out", "unknown":
				span.SetStatus(codes.Error, outcome)
			}

			if operation != "agent" {
				o.toolCalls.WithLabelValues(operation, phase, outcome).Inc()
				return
			}

			switch outcome {
			case "failed", "timed_out", "unknown":
				o.agentFailures.WithLabelValues(phase, reason).Inc()
			case "cancelled":
				o.cancellations.WithLabelValues(phase).Inc()
			}
		})
	}
}

func executionValue(value, fallback string, allowed ...string) string {
	for _, candidate := range allowed {
		if value == candidate {
			return value
		}
	}
	return fallback
}

func RecordAgentMessageSave(ctx context.Context, businessEffect string, saved bool) {
	outcome := "unconfirmed"
	if saved {
		outcome = "saved"
	}

	effect := executionValue(businessEffect, "none",
		"none", "committed", "unknown",
	)

	trace.SpanFromContext(ctx).AddEvent(
		"agent.message_save",
		trace.WithAttributes(
			attribute.String("app.outcome", outcome),
			attribute.String("app.business_effect", effect),
		),
	)
}
