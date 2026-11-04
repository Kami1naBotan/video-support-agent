package service

import (
	"context"
	"time"

	"video-support-agent/internal/llm"
	"video-support-agent/internal/model"
)

type AgentConfirmation struct {
	Input        CreateAppealInput       `json:"-"`
	Confirmation AppealConfirmationInput `json:"-"`
}

type AgentRequest struct {
	UserID         uint64               `json:"-"`
	ConversationID uint64               `json:"-"`
	History        []llm.HistoryMessage `json:"-"`
	Question       string               `json:"-"`
	Confirmed      *AgentConfirmation   `json:"-"`
}

type AgentTicketStatus struct {
	TicketID  string                   `json:"ticket_id"`
	Status    model.AppealTicketStatus `json:"status"`
	CreatedAt time.Time                `json:"created_at"`
	UpdatedAt time.Time                `json:"updated_at"`
}

type AgentExecutor interface {
	ConfirmedAgentExecutor
	Execute(context.Context, AgentRequest, AgentEventSink) (*AgentOutcome, error)
}
