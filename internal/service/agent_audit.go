package service

import (
	"context"
	"errors"
	"strings"

	"video-support-agent/internal/model"
)

var (
	ErrAgentAuditConfiguration = errors.New("agent audit configuration invalid")
	ErrAgentAuditUnavailable   = errors.New("agent audit unavailable")
	ErrAgentAuditConflict      = errors.New("agent audit state conflict")
	ErrAgentRunNotFound        = errors.New("agent run not found")
)

type AgentAuditOwner struct {
	UserID         uint64
	ConversationID uint64
}

func (o AgentAuditOwner) Validate() error {
	if o.UserID == 0 {
		return ErrUnauthorized
	}
	if o.ConversationID == 0 {
		return ErrInvalidArgument
	}
	return nil
}

type AgentCallStart struct {
	ToolName      string
	ModelCallID   string // 确认直达时为空，数据库保存NULL。
	ArgumentBytes uint32
	Binding       *AppealConfirmationBinding // 仅结构化确认创建时提供。
}

type AgentCallFinish struct {
	Status         string
	ErrorCode      string
	BusinessEffect string
	Ticket         *AppealCreateResult
}

type AgentRunFinish struct {
	Status     string
	Decision   string
	ReasonCode string
	ModelCalls uint32
}

type AgentAuditSnapshot struct {
	Run   model.AgentRun   `json:"-"`
	Calls []model.ToolCall `json:"-"`
}

type AgentAuditStore interface {
	StartRun(context.Context, AgentAuditOwner) (*model.AgentRun, error)
	StartCall(context.Context, AgentAuditOwner, string, AgentCallStart) (*model.ToolCall, error)
	AuthorizeCreate(context.Context, AgentAuditOwner, string, string, AppealConfirmationBinding) error
	FinishCall(context.Context, AgentAuditOwner, string, string, AgentCallFinish) error
	FinishRun(context.Context, AgentAuditOwner, string, AgentRunFinish) error
	ReadOwned(context.Context, AgentAuditOwner, string) (*AgentAuditSnapshot, error)
}

func NewAgentAuditID() (string, error) {
	return newAppealOperationKey()
}

func AgentAuditTool(name string) bool {
	switch name {
	case "search_rule", "get_rule_detail", "check_appeal_eligibility", "create_appeal_ticket", "query_ticket_status":
		return true
	}
	return false
}

func AgentAuditCode(code string) bool {
	return strings.Contains(
		"|none|invalid_argument|unauthorized|not_found|configuration_error|invalid_result|result_too_large|unavailable|timed_out|cancelled|outcome_unknown|audit_unavailable|create_already_attempted|idempotency_conflict|confirmation_required|precondition_changed|needs_more_info|ineligible|no_evidence|manual_review|protocol_error|budget_exhausted|repeated_call|unsupported|event_failed|model_failed|",
		"|"+code+"|",
	) && code != "" && !strings.Contains(code, "|")
}

func ValidateAgentCallStart(owner AgentAuditOwner, c AgentCallStart) error {
	if err := owner.Validate(); err != nil {
		return err
	}
	if !AgentAuditTool(c.ToolName) || c.ArgumentBytes == 0 || c.ArgumentBytes > 16*1024 {
		return ErrInvalidArgument
	}

	if len(c.ModelCallID) > 128 {
		return ErrInvalidArgument
	}
	for i := range c.ModelCallID {
		if c.ModelCallID[i] < 0x21 || c.ModelCallID[i] > 0x7e {
			return ErrInvalidArgument
		}
	}

	if c.Binding == nil {
		if c.ModelCallID == "" {
			return ErrInvalidArgument
		}
		return nil
	}

	if c.ModelCallID != "" || c.ToolName != "create_appeal_ticket" || c.Binding.UserID != owner.UserID || c.Binding.ConversationID != owner.ConversationID {
		return ErrInvalidArgument
	}
	return ValidateAppealConfirmationBinding(*c.Binding)
}

func ValidateAgentCallFinish(c AgentCallFinish) error {
	switch c.Status {
	case "succeeded", "rejected", "failed", "cancelled", "timed_out", "unknown":
	default:
		return ErrInvalidArgument
	}

	if !AgentAuditCode(c.ErrorCode) {
		return ErrInvalidArgument
	}
	if (c.Status == "succeeded") != (c.ErrorCode == "none") {
		return ErrInvalidArgument
	}

	switch c.BusinessEffect {
	case "none":
		if c.Ticket != nil || c.Status == "unknown" {
			return ErrInvalidArgument
		}

	case "unknown":
		if c.Ticket != nil || c.Status != "unknown" || c.ErrorCode != "outcome_unknown" {
			return ErrInvalidArgument
		}

	case "committed":
		if c.Ticket == nil || c.Ticket.TicketID == 0 || !validAppealTime(c.Ticket.CreatedAt) || c.Status != "succeeded" {
			return ErrInvalidArgument
		}

	default:
		return ErrInvalidArgument
	}

	return nil
}

func ValidateAgentRunFinish(f AgentRunFinish) error {
	switch f.Status {
	case "completed", "failed", "cancelled", "timed_out", "unknown":
	default:
		return ErrInvalidArgument
	}

	switch f.Decision {
	case "answer", "clarify", "handoff", "confirmation_required", "ticket_created", "ticket_status", "failed":
	default:
		return ErrInvalidArgument
	}

	if !AgentAuditCode(f.ReasonCode) || f.ModelCalls > 6 {
		return ErrInvalidArgument
	}
	return nil
}

type AgentAuditObservation struct {
	Stored         *AgentAuditSnapshot `json:"-"`
	BusinessEffect string              `json:"-"`
	Ticket         *AppealCreateResult `json:"-"`
	RecoveryCode   string              `json:"-"`
}

type AgentAppealRecovery interface {
	Recover(context.Context, uint64, uint64, string, string) (*AppealCreateResult, error)
}

func ObserveAgentRun(ctx context.Context, store AgentAuditStore, appeals AgentAppealRecovery, owner AgentAuditOwner, runID string) (*AgentAuditObservation, error) {
	if store == nil || appeals == nil {
		return nil, ErrAgentAuditConfiguration
	}

	snapshot, err := store.ReadOwned(ctx, owner, runID)
	if err != nil {
		return nil, err
	}
	if snapshot == nil {
		return nil, ErrAgentAuditUnavailable
	}

	result := &AgentAuditObservation{
		Stored:         snapshot,
		BusinessEffect: snapshot.Run.BusinessEffect,
		RecoveryCode:   "none",
	}

	for _, call := range snapshot.Calls {
		if !call.WriteAuthorized {
			continue
		}

		if call.BusinessEffect == "committed" &&
			call.TicketID != nil &&
			call.TicketCreatedAt != nil {
			result.Ticket = &AppealCreateResult{
				TicketID:      *call.TicketID,
				InitialStatus: call.TicketStatus,
				CreatedAt:     *call.TicketCreatedAt,
				Replayed:      call.Replayed,
			}
			result.BusinessEffect = "committed"
		}

		ticket, readErr := appeals.Recover(ctx, owner.UserID, owner.ConversationID, call.OperationKey, call.RequestHash)
		if readErr == nil && ticket != nil {
			result.Ticket = ticket
			result.BusinessEffect = "committed"
			return result, nil
		}

		result.RecoveryCode = "outcome_unknown"
		if errors.Is(readErr, ErrAppealIdempotencyConflict) {
			result.RecoveryCode = "idempotency_conflict"
		}

		if call.BusinessEffect == "committed" {
			result.BusinessEffect = "committed"
		} else if call.Status == "started" || call.BusinessEffect == "unknown" {
			result.BusinessEffect = "unknown"
		}
	}

	return result, nil
}
