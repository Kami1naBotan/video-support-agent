package handler

import (
	"strconv"

	"video-support-agent/internal/service"
)

type agentStartEvent struct {
	RunID string `json:"run_id"`
}

type agentToolEvent struct {
	RunID          string `json:"run_id"`
	CallID         string `json:"call_id"`
	Sequence       uint32 `json:"sequence"`
	ToolName       string `json:"tool_name"`
	Status         string `json:"status"`
	ErrorCode      string `json:"error_code,omitempty"`
	BusinessEffect string `json:"business_effect"`
	AuditPending   bool   `json:"audit_pending"`
	TicketID       string `json:"ticket_id,omitempty"`
	Replayed       bool   `json:"replayed"`
	DurationMS     *int64 `json:"duration_ms,omitempty"`
}

type agentStreamError struct {
	ErrorResponse

	RunID    string                       `json:"run_id,omitempty"`
	Agent    *agentResultView             `json:"agent,omitempty"`
	Messages *service.AgentMessageReceipt `json:"messages,omitempty"`
}

type agentStreamDone struct {
	RunID              string           `json:"run_id"`
	UserMessageID      uint64           `json:"user_message_id"`
	AssistantMessageID uint64           `json:"assistant_message_id"`
	MessageSaveStatus  string           `json:"message_save_status"`
	Agent              *agentResultView `json:"agent"`
}

func agentStreamProtocolError() error {
	return &service.AgentExecutionError{Code: "protocol_error"}
}

func projectAgentStreamEvent(event service.AgentEvent) (SSEEvent, any, error) {
	bad := agentStreamProtocolError()

	if service.ValidateAppealOperationKey(event.RunID) != nil {
		return "", nil, bad
	}

	if event.Kind == "agent_start" {
		return SSEEventAgentStart, agentStartEvent{
			RunID: event.RunID,
		}, nil
	}

	if event.Kind != "tool_start" && event.Kind != "tool_end" {
		return "", nil, bad
	}

	if service.ValidateAppealOperationKey(event.CallID) != nil || event.Sequence == 0 || event.Sequence > 5 || !service.AgentAuditTool(event.ToolName) {
		return "", nil, bad
	}

	view := agentToolEvent{
		RunID:          event.RunID,
		CallID:         event.CallID,
		Sequence:       event.Sequence,
		ToolName:       event.ToolName,
		Status:         event.Status,
		BusinessEffect: event.BusinessEffect,
		AuditPending:   event.AuditPending,
	}

	if event.Kind == "tool_start" {
		if event.Status != "started" || event.BusinessEffect != "none" {
			return "", nil, bad
		}
		return SSEEventToolStart, view, nil
	}

	switch event.Status {
	case "succeeded", "rejected", "failed", "cancelled", "timed_out", "unknown":
	default:
		return "", nil, bad
	}

	switch event.BusinessEffect {
	case "none", "committed", "unknown":
	default:
		return "", nil, bad
	}

	if !service.AgentAuditCode(event.ErrorCode) || event.DurationMS == nil || *event.DurationMS < 0 {
		return "", nil, bad
	}

	if event.TicketID != "" {
		id, err := strconv.ParseUint(event.TicketID, 10, 64)
		if err != nil || id == 0 || strconv.FormatUint(id, 10) != event.TicketID {
			return "", nil, bad
		}
	} else if event.Replayed || event.BusinessEffect == "committed" {
		return "", nil, bad
	}

	duration := *event.DurationMS
	view.ErrorCode = event.ErrorCode
	view.TicketID = event.TicketID
	view.Replayed = event.Replayed
	view.DurationMS = &duration

	return SSEEventToolEnd, view, nil
}
