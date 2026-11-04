package service

import (
	"context"

	"video-support-agent/internal/model"
)

type AgentConfirmedRequest struct {
	UserID         uint64                  `json:"-"`
	ConversationID uint64                  `json:"-"`
	Input          CreateAppealInput       `json:"-"`
	Confirmation   AppealConfirmationInput `json:"-"`
}

type AgentToolSummary struct {
	CallID         string `json:"call_id"`
	Sequence       uint32 `json:"sequence"`
	ToolName       string `json:"tool_name"`
	Status         string `json:"status"`
	ErrorCode      string `json:"error_code"`
	BusinessEffect string `json:"business_effect"`
	AuditPending   bool   `json:"audit_pending"`
}

type AgentOutcome struct {
	RunID             string               `json:"run_id,omitempty"`
	ExecutionStatus   string               `json:"execution_status"`
	Decision          string               `json:"decision"`
	ReasonCode        string               `json:"reason_code"`
	BusinessEffect    string               `json:"business_effect"`
	MessageSaveStatus string               `json:"message_save_status"`
	AuditPending      bool                 `json:"audit_pending"`
	Content           string               `json:"content"`
	Ticket            *AppealCreateResult  `json:"ticket,omitempty"`
	TicketStatus      *AgentTicketStatus   `json:"ticket_status,omitempty"`
	Citations         []model.RuleCitation `json:"citations"`
	Tools             []AgentToolSummary   `json:"tools"`

	Proposal *PreparedAppeal `json:"-"`
}

type AgentEvent struct {
	Kind           string `json:"kind"`
	RunID          string `json:"run_id"`
	CallID         string `json:"call_id,omitempty"`
	Sequence       uint32 `json:"sequence,omitempty"`
	ToolName       string `json:"tool_name,omitempty"`
	Status         string `json:"status,omitempty"`
	ErrorCode      string `json:"error_code,omitempty"`
	BusinessEffect string `json:"business_effect,omitempty"`
	TicketID       string `json:"ticket_id,omitempty"`
	Replayed       bool   `json:"replayed,omitempty"`
	AuditPending   bool   `json:"audit_pending"`
	DurationMS     *int64 `json:"duration_ms,omitempty"`
}

type AgentEventSink func(context.Context, AgentEvent) error

type ConfirmedAgentExecutor interface {
	Confirm(context.Context, AgentConfirmedRequest, AgentEventSink) (*AgentOutcome, error)
	Recover(context.Context, AgentAuditOwner, string) (*AgentAuditObservation, error)
}

type AgentExecutionError struct {
	Code string
}

func (e *AgentExecutionError) Error() string {
	return e.Code
}
