package evaluation

import (
	"context"
	"encoding/json"

	"video-support-agent/internal/service"
	"video-support-agent/internal/tools"
)

type ToolCapture struct {
	trace *tools.EvaluationTrace
}

func NewToolCapture(ctx context.Context, corpus *Corpus) (context.Context, *ToolCapture, error) {
	if corpus == nil || !corpus.Synthetic() {
		return nil, nil, fieldError("tool_capture", "requires an isolated synthetic evaluation")
	}

	observedCtx, trace, err := tools.NewEvaluationTrace(ctx)
	if err != nil {
		return nil, nil, fieldError("tool_capture", "cannot attach trace")
	}
	return observedCtx, &ToolCapture{trace: trace}, nil
}

func (c *ToolCapture) Finish(out *service.AgentOutcome) ([]ObservedTool, error) {
	if c == nil || c.trace == nil {
		return nil, fieldError("tool_capture", "is required")
	}

	calls, err := c.trace.Take()
	if err != nil {
		return nil, fieldError("tool_capture", "trace is incomplete")
	}
	defer clear(calls)

	if out == nil {
		return nil, fieldError("tool_capture", "outcome is missing")
	}
	if out.AuditPending {
		return nil, fieldError("tool_capture", "audit is pending")
	}
	if len(out.Tools) > 5 {
		return nil, fieldError("tool_capture", "too many summaries")
	}

	summaries := make(map[string]service.AgentToolSummary, len(out.Tools))
	var lastSequence uint32

	for _, summary := range out.Tools {
		if service.ValidateAppealOperationKey(summary.CallID) != nil ||
			summary.Sequence <= lastSequence ||
			summary.Sequence > 5 ||
			summary.AuditPending ||
			!validCapturedStatus(summary.Status) ||
			!oneOf(summary.BusinessEffect, "none", "unknown", "committed") {
			return nil, fieldError("tool_capture", "invalid summary")
		}
		if _, exists := summaries[summary.CallID]; exists {
			return nil, fieldError("tool_capture", "duplicate summary")
		}
		summaries[summary.CallID] = summary
		lastSequence = summary.Sequence
	}

	observed := make([]ObservedTool, 0, len(calls))
	for _, call := range calls {
		summary, exists := summaries[call.Identity.CallID]
		if !exists ||
			call.Identity.RunID != out.RunID ||
			call.Identity.Sequence != summary.Sequence ||
			call.Name != summary.ToolName {
			return nil, fieldError("tool_capture", "invocation and summary mismatch")
		}

		arguments, err := capturedArguments(call.Name, call.Arguments)
		if err != nil {
			return nil, err
		}

		observed = append(observed, ObservedTool{
			Name:           call.Name,
			Arguments:      arguments,
			Status:         summary.Status,
			BusinessEffect: summary.BusinessEffect,
		})
		delete(summaries, call.Identity.CallID)
	}

	for _, summary := range summaries {
		if out.ExecutionStatus == "completed" || summary.Status == "succeeded" || summary.BusinessEffect != "none" {
			return nil, fieldError("tool_capture", "summary has no actual invocation")
		}
	}

	return observed, nil
}

func validCapturedStatus(status string) bool {
	return oneOf(status, "succeeded", "rejected", "failed", "cancelled", "timed_out", "unknown")
}

func capturedArguments(name string, raw string) (map[string]json.RawMessage, error) {
	if _, err := argumentValue(json.RawMessage(raw)); err != nil {
		return nil, fieldError("tool_capture", "invalid argument JSON")
	}

	var arguments map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &arguments); err != nil ||
		arguments == nil {
		return nil, fieldError("tool_capture", "arguments must be an object")
	}
	if err := validateArguments(arguments); err != nil {
		return nil, fieldError("tool_capture", "arguments exceed evaluation limits")
	}

	for key := range arguments {
		if !capturedFieldAllowed(name, key) {
			return nil, fieldError("tool_capture", "unexpected argument field")
		}
	}
	return arguments, nil
}

func capturedFieldAllowed(name, field string) bool {
	switch name {
	case tools.SearchRuleName:
		return oneOf(field, "query", "top_k")

	case tools.GetRuleDetailName:
		return oneOf(field, "mode", "document_id", "version_id", "article_id")

	case tools.CheckAppealEligibilityName:
		return oneOf(field, "subject_type", "subject_ref", "reason")

	case tools.CreateAppealTicketName:
		return oneOf(field, "subject_type", "subject_ref", "reason", "intake_mode")

	case tools.QueryTicketStatusName:
		return field == "ticket_id"

	default:
		return false
	}
}
