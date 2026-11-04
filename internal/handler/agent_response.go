package handler

import (
	"errors"
	"net/http"

	"video-support-agent/internal/service"
)

func classifyAgentResponseError(err error) (int, ErrorResponse, bool) {
	code := ""

	switch {
	case errors.Is(err, service.ErrAgentMessageSave):
		code = "message_save_unconfirmed"
	case errors.Is(err, service.ErrAgentRunNotFound):
		code = "agent_run_not_found"
	case errors.Is(err, service.ErrAppealTicketNotFound):
		code = "appeal_ticket_not_found"
	case errors.Is(err, service.ErrAgentAuditConfiguration),
		errors.Is(err, service.ErrAppealConfiguration):
		code = "configuration_error"
	case errors.Is(err, service.ErrAgentAuditUnavailable):
		code = "audit_unavailable"
	case errors.Is(err, service.ErrAgentAuditConflict):
		code = "audit_conflict"
	case errors.Is(err, service.ErrAppealIdempotencyConflict):
		code = "idempotency_conflict"
	case errors.Is(err, service.ErrAppealConfirmationRequired):
		code = "confirmation_required"
	case errors.Is(err, service.ErrAppealPreconditionChanged):
		code = "precondition_changed"
	case errors.Is(err, service.ErrAppealNeedsMoreInfo):
		code = "needs_more_info"
	case errors.Is(err, service.ErrAppealIneligible):
		code = "ineligible"
	case errors.Is(err, service.ErrAppealOutcomeUnknown):
		code = "outcome_unknown"
	case errors.Is(err, service.ErrAppealRetryableStorage),
		errors.Is(err, service.ErrAppealStorage):
		code = "unavailable"
	case errors.Is(err, service.ErrAppealInvalidSnapshot),
		errors.Is(err, service.ErrAppealInvalidPolicy):
		code = "invalid_result"
	default:
		var execution *service.AgentExecutionError
		if !errors.As(err, &execution) {
			return 0, ErrorResponse{}, false
		}
		code = execution.Code
	}

	status := http.StatusServiceUnavailable
	response := ErrorResponse{
		Code:    "agent_unavailable",
		Message: "本次执行未能正常完成，请先核对已有结果，不要自动重复提交。",
	}

	switch code {
	case "invalid_argument":
		status = http.StatusBadRequest
		response = ErrorResponse{"invalid_argument", "请求参数无效。"}
	case "needs_more_info":
		status = http.StatusBadRequest
		response = ErrorResponse{"appeal_needs_more_info", "请补全申请信息。"}
	case "unauthorized":
		status = http.StatusUnauthorized
		response = ErrorResponse{"unauthorized", "authentication required"}
	case "not_found", "agent_run_not_found", "appeal_ticket_not_found":
		status = http.StatusNotFound
		response = ErrorResponse{"agent_resource_not_found", "请求的资源不存在或不可访问。"}
	case "idempotency_conflict":
		status = http.StatusConflict
		response = ErrorResponse{"appeal_idempotency_conflict", "原操作Key对应的申请内容不一致。"}
	case "confirmation_required":
		status = http.StatusConflict
		response = ErrorResponse{"appeal_confirmation_required", "确认凭据无效或已失效，请重新核对确认卡片。"}
	case "precondition_changed":
		status = http.StatusConflict
		response = ErrorResponse{"appeal_precondition_changed", "业务条件已变化，请重新准备确认。"}
	case "create_already_attempted", "audit_conflict":
		status = http.StatusConflict
		response = ErrorResponse{"agent_state_conflict", "执行状态冲突，请先查询恢复结果。"}
	case "ineligible":
		status = http.StatusUnprocessableEntity
		response = ErrorResponse{"appeal_ineligible", "当前资格不允许创建该工单。"}
	case "timed_out":
		status = http.StatusGatewayTimeout
		response = ErrorResponse{"agent_timeout", "执行超时，请先核对已有结果。"}
	case "cancelled":
		status = http.StatusRequestTimeout
		response = ErrorResponse{"agent_cancelled", "执行已取消，请核对业务及消息保存状态。"}
	case "message_save_unconfirmed":
		response = ErrorResponse{"agent_message_save_unconfirmed", "聊天消息保存尚未确认，请查询恢复结果。"}
	case "outcome_unknown":
		response = ErrorResponse{"appeal_outcome_unknown", "工单提交结果待确认，请保留原操作并查询恢复结果。"}
	case "configuration_error":
		response = ErrorResponse{"agent_unavailable", "Agent服务尚不可用。"}
	case "audit_unavailable":
		response = ErrorResponse{"agent_audit_unavailable", "执行记录暂时无法完整核对，请保留恢复标识。"}
	case "invalid_result", "result_too_large", "protocol_error":
		status = http.StatusBadGateway
		response = ErrorResponse{"agent_invalid_result", "执行结果未通过协议校验。"}
	case "model_failed":
		status = http.StatusBadGateway
		response = ErrorResponse{"agent_model_failed", "模型调用未能正常完成。"}
	}

	return status, response, true
}
