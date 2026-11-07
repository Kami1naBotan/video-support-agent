package tools

import (
	"errors"

	"video-support-agent/internal/observability"
)

func observedToolResult(err error) observability.ExecutionResult {
	result := observability.ExecutionResult{
		Outcome:        "success",
		Reason:         "none",
		BusinessEffect: "none",
	}
	if err == nil {
		return result
	}

	result.Outcome = "failed"
	result.Reason = "unavailable"

	var failure *Failure
	if errors.As(err, &failure) {
		result.Reason = failure.Code
	} else {
		var normalized *Failure
		if errors.As(fail(err), &normalized) {
			result.Reason = normalized.Code
		}
	}

	switch result.Reason {
	case "timed_out", "cancelled":
		result.Outcome = result.Reason
	case "outcome_unknown":
		result.Outcome = "unknown"
	case "invalid_argument", "unauthorized", "not_found",
		"confirmation_required", "precondition_changed",
		"needs_more_info", "ineligible", "idempotency_conflict",
		"create_already_attempted":
		result.Outcome = "rejected"
	}

	return result
}
