package agent

import (
	"context"
	"errors"

	"video-support-agent/internal/model"
	"video-support-agent/internal/service"
	"video-support-agent/internal/tools"
)

func errorCode(err error) string {
	if err == nil {
		return "none"
	}
	if errors.Is(err, service.ErrAppealOutcomeUnknown) {
		return "outcome_unknown"
	}

	var toolFailure *tools.Failure
	if errors.As(err, &toolFailure) &&
		service.AgentAuditCode(toolFailure.Code) {
		return toolFailure.Code
	}

	var execution *service.AgentExecutionError
	if errors.As(err, &execution) &&
		service.AgentAuditCode(execution.Code) {
		return execution.Code
	}

	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timed_out"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, service.ErrUnauthorized):
		return "unauthorized"
	case errors.Is(err, service.ErrInvalidArgument):
		return "invalid_argument"
	case errors.Is(err, service.ErrAppealNeedsMoreInfo):
		return "needs_more_info"
	case errors.Is(err, service.ErrConversationNotFound),
		errors.Is(err, service.ErrAgentRunNotFound):
		return "not_found"
	case errors.Is(err, service.ErrAgentAuditConfiguration):
		return "configuration_error"
	case errors.Is(err, service.ErrAgentAuditUnavailable),
		errors.Is(err, service.ErrAgentAuditConflict):
		return "audit_unavailable"
	default:
		return "unavailable"
	}
}

func safeError(err error) error {
	if err == nil {
		return nil
	}
	return &service.AgentExecutionError{Code: errorCode(err)}
}

func callFailure(code string) string {
	switch code {
	case "cancelled", "timed_out":
		return code
	case "outcome_unknown":
		return "unknown"
	case "invalid_argument",
		"unauthorized",
		"not_found",
		"confirmation_required",
		"precondition_changed",
		"needs_more_info",
		"ineligible",
		"idempotency_conflict":
		return "rejected"
	default:
		return "failed"
	}
}

func projectOutcome(out *service.AgentOutcome, err error) {
	out.ReasonCode = errorCode(err)
	out.ExecutionStatus = "completed"

	if err != nil {
		out.ExecutionStatus = "failed"
		if out.ReasonCode == "cancelled" || out.ReasonCode == "timed_out" {
			out.ExecutionStatus = out.ReasonCode
		}
	}

	switch out.BusinessEffect {
	case "committed":
		out.Decision = "ticket_created"
		out.Content = "本项目申诉工单已保存。"

		if out.Ticket != nil && out.Ticket.InitialStatus == model.AppealTicketPendingReview {
			out.Content = "本项目人工复核工单已保存，等待人工复核；不代表平台已受理。"
		}

	case "unknown":
		out.ExecutionStatus = "unknown"
		out.Decision = "failed"
		out.ReasonCode = "outcome_unknown"
		out.Content = "工单提交结果待确认，请保留原操作并查询恢复结果，不要换键重复创建。"

	default:
		out.Decision = "failed"
		out.Content = "本次确认执行未完成，请保留原操作信息。"

		switch out.ReasonCode {
		case "confirmation_required", "precondition_changed":
			out.Content = "确认凭据或业务条件需要重新核对，当前请求未创建工单。"
		case "needs_more_info":
			out.Decision = "clarify"
			out.Content = "请补全申诉事项和申请说明后重新准备确认。"
		case "ineligible":
			out.Content = "当前资格检查不允许创建该工单。"
		}
	}

	if out.AuditPending {
		out.Content += " 本次执行的审计记录尚待核对。"
	}
}

func emit(ctx context.Context, sink service.AgentEventSink, event service.AgentEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if sink == nil {
		return nil
	}

	if err := sink(ctx, event); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &service.AgentExecutionError{Code: "event_failed"}
	}
	return ctx.Err()
}
