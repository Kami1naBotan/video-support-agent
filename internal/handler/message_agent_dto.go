package handler

import (
	"strconv"
	"time"

	"video-support-agent/internal/model"
	"video-support-agent/internal/service"
)

type agentResultView struct {
	RunID             string                          `json:"run_id,omitempty"`
	ExecutionStatus   string                          `json:"execution_status"`
	Decision          string                          `json:"decision"`
	ReasonCode        string                          `json:"reason_code"`
	BusinessEffect    string                          `json:"business_effect"`
	MessageSaveStatus string                          `json:"message_save_status"`
	AuditPending      bool                            `json:"audit_pending"`
	Content           string                          `json:"content"`
	Ticket            *service.AgentCreatedTicketView `json:"ticket,omitempty"`
	TicketStatus      *service.AgentTicketStatus      `json:"ticket_status,omitempty"`
	Citations         []model.RuleCitation            `json:"citations"`
	Tools             []service.AgentToolSummary      `json:"tools"`
}

type appealActionView struct {
	RunID           string                          `json:"run_id"`
	ConfirmedAction confirmedActionRequest          `json:"confirmed_action"`
	Eligibility     model.AppealEligibilityDecision `json:"eligibility"`
	ReasonCode      string                          `json:"reason_code"`
	InitialStatus   model.AppealTicketStatus        `json:"initial_status"`
	IssuedAt        time.Time                       `json:"issued_at"`
	ExpiresAt       time.Time                       `json:"expires_at"`
}

type agentMessageResponse struct {
	*model.Message

	Agent          *agentResultView             `json:"agent,omitempty"`
	Messages       *service.AgentMessageReceipt `json:"messages,omitempty"`
	ActionRequired *appealActionView            `json:"action_required,omitempty"`
	Code           string                       `json:"code,omitempty"`
	ErrorMessage   string                       `json:"message,omitempty"`
}

func projectAgentResult(out *service.AgentOutcome) *agentResultView {
	if out == nil {
		return nil
	}

	view := &agentResultView{
		RunID:             out.RunID,
		ExecutionStatus:   out.ExecutionStatus,
		Decision:          out.Decision,
		ReasonCode:        out.ReasonCode,
		BusinessEffect:    out.BusinessEffect,
		MessageSaveStatus: out.MessageSaveStatus,
		AuditPending:      out.AuditPending,
		Content:           out.Content,
		Citations:         append([]model.RuleCitation{}, out.Citations...),
		Tools:             append([]service.AgentToolSummary{}, out.Tools...),
	}

	if out.Ticket != nil {
		view.Ticket = &service.AgentCreatedTicketView{
			TicketID:      strconv.FormatUint(out.Ticket.TicketID, 10),
			InitialStatus: out.Ticket.InitialStatus,
			CreatedAt:     out.Ticket.CreatedAt,
			Replayed:      out.Ticket.Replayed,
		}
	}
	if out.TicketStatus != nil {
		status := *out.TicketStatus
		view.TicketStatus = &status
	}

	return view
}

func projectAppealAction(out *service.AgentOutcome) *appealActionView {
	if out == nil ||
		out.Proposal == nil ||
		out.Decision != "confirmation_required" ||
		out.BusinessEffect != "none" {
		return nil
	}

	proposal := out.Proposal
	return &appealActionView{
		RunID: out.RunID,
		ConfirmedAction: confirmedActionRequest{
			SubjectType:       proposal.Input.SubjectType,
			SubjectRef:        proposal.Input.SubjectRef,
			Reason:            proposal.Input.Reason,
			IntakeMode:        proposal.Input.IntakeMode,
			IdempotencyKey:    proposal.Binding.IdempotencyKey,
			PolicyDigest:      proposal.Binding.PolicyDigest,
			ConfirmationToken: proposal.Token,
		},
		Eligibility:   proposal.Eligibility.Decision,
		ReasonCode:    proposal.Eligibility.ReasonCode,
		InitialStatus: proposal.InitialStatus,
		IssuedAt:      proposal.IssuedAt,
		ExpiresAt:     proposal.ExpiresAt,
	}
}
