package agent

import (
	"context"

	"video-support-agent/internal/observability"
	"video-support-agent/internal/service"
)

type observedExecutor struct {
	service.AgentExecutor
	observer *observability.ExecutionObserver
}

var _ service.AgentExecutor = (*observedExecutor)(nil)

func ObserveExecutor(executor service.AgentExecutor, observer *observability.ExecutionObserver) (service.AgentExecutor, error) {
	if nilValue(executor) {
		return nil, service.ErrAgentAuditConfiguration
	}
	if observer == nil {
		return executor, nil
	}
	return &observedExecutor{
		AgentExecutor: executor,
		observer:      observer,
	}, nil
}

func (e *observedExecutor) Execute(ctx context.Context, request service.AgentRequest, sink service.AgentEventSink) (out *service.AgentOutcome, returnErr error) {
	phase := "chat"
	if request.Confirmed != nil {
		phase = "confirm"
	}

	ctx, finish := e.observer.Start(ctx, "agent", phase)
	returned := false
	defer func() {
		finish(observedAgentResult(out, returnErr, returned))
	}()

	out, returnErr = e.AgentExecutor.Execute(ctx, request, sink)
	returned = true
	return
}

func (e *observedExecutor) Confirm(ctx context.Context, request service.AgentConfirmedRequest, sink service.AgentEventSink) (out *service.AgentOutcome, returnErr error) {
	ctx, finish := e.observer.Start(ctx, "agent", "confirm")
	returned := false
	defer func() {
		finish(observedAgentResult(out, returnErr, returned))
	}()

	out, returnErr = e.AgentExecutor.Confirm(ctx, request, sink)
	returned = true
	return
}

func observedAgentResult(out *service.AgentOutcome, err error, returned bool) observability.ExecutionResult {
	result := observability.ExecutionResult{
		Outcome:        "failed",
		Reason:         "invalid_result",
		BusinessEffect: "none",
	}

	if out != nil {
		result.Outcome = out.ExecutionStatus
		result.Reason = out.ReasonCode
		result.BusinessEffect = out.BusinessEffect
		result.AuditPending = out.AuditPending
	}

	if !returned {
		result.Outcome = "failed"
		result.Reason = "panic"
		return result
	}

	if result.BusinessEffect == "unknown" {
		result.Outcome = "unknown"
		result.Reason = "outcome_unknown"
		return result
	}

	if err != nil {
		result.Outcome = "failed"
		result.Reason = errorCode(err)
		switch result.Reason {
		case "timed_out", "cancelled":
			result.Outcome = result.Reason
		case "outcome_unknown":
			result.Outcome = "unknown"
		}
	} else if result.AuditPending {
		result.Outcome = "failed"
		result.Reason = "audit_unavailable"
	}

	return result
}
