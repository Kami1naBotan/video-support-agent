package service

import (
	"context"
	"strconv"
	"time"

	"video-support-agent/internal/model"
)

const AgentReadBatchSize = 50

type AgentRunSummary struct {
	RunID             string     `json:"run_id"`
	ExecutionStatus   string     `json:"execution_status"`
	Decision          string     `json:"decision"`
	ReasonCode        string     `json:"reason_code"`
	BusinessEffect    string     `json:"business_effect"`
	MessageSaveStatus string     `json:"message_save_status"`
	StartedAt         time.Time  `json:"started_at"`
	FinishedAt        *time.Time `json:"finished_at,omitempty"`
}

type AgentMessageReceipt struct {
	UserMessageID      uint64 `json:"user_message_id"`
	AssistantMessageID uint64 `json:"assistant_message_id"`
}

type AgentCreatedTicketView struct {
	TicketID      string                   `json:"ticket_id"`
	InitialStatus model.AppealTicketStatus `json:"initial_status"`
	CreatedAt     time.Time                `json:"created_at"`
	Replayed      bool                     `json:"replayed"`
}

type AgentRecoveryView struct {
	Stored            AgentRunSummary         `json:"stored"`
	BusinessEffect    string                  `json:"business_effect"`
	RecoveryCode      string                  `json:"recovery_code"`
	AuditPending      bool                    `json:"audit_pending"`
	MessageSaveStatus string                  `json:"message_save_status"`
	Messages          *AgentMessageReceipt    `json:"messages,omitempty"`
	Ticket            *AgentCreatedTicketView `json:"ticket,omitempty"`
	Tools             []AgentToolSummary      `json:"tools"`
}

type MessageHistoryView struct {
	model.Message
	Agent *AgentRunSummary `json:"agent,omitempty"`
}

type AgentReadStore interface {
	ListRunsOwned(context.Context, AgentAuditOwner, []string) ([]model.AgentRun, error)
	ReadMessageReceipt(context.Context, AgentAuditOwner, string) (*AgentMessageReceipt, error)
}

type AgentRecoveryReader interface {
	Recover(context.Context, AgentAuditOwner, string) (*AgentAuditObservation, error)
}

type AgentReadService struct {
	conversations ConversationStore
	messages      MessageStore
	audits        AgentReadStore
	recovery      AgentRecoveryReader
}

func NewAgentReadService(conversations ConversationStore, messages MessageStore, audits AgentReadStore, recovery AgentRecoveryReader) (*AgentReadService, error) {
	if conversations == nil || messages == nil || audits == nil || recovery == nil {
		return nil, ErrAgentAuditConfiguration
	}

	return &AgentReadService{
		conversations: conversations,
		messages:      messages,
		audits:        audits,
		recovery:      recovery,
	}, nil
}

func (s *AgentReadService) checkOwner(ctx context.Context, owner AgentAuditOwner) error {
	if err := owner.Validate(); err != nil {
		return err
	}

	conversation, err := s.conversations.GetByID(ctx, owner.ConversationID)
	if err != nil {
		return normalizeConversationError(err)
	}
	if conversation == nil || conversation.UserID != owner.UserID {
		return ErrConversationNotFound
	}
	return nil
}

func summarizeAgentRun(run model.AgentRun) AgentRunSummary {
	return AgentRunSummary{
		RunID:             run.ID,
		ExecutionStatus:   run.ExecutionStatus,
		Decision:          run.Decision,
		ReasonCode:        run.ReasonCode,
		BusinessEffect:    run.BusinessEffect,
		MessageSaveStatus: run.MessageSaveStatus,
		StartedAt:         run.StartedAt,
		FinishedAt:        run.FinishedAt,
	}
}

func (s *AgentReadService) ListHistory(ctx context.Context, userID uint64, conversationID uint64) ([]MessageHistoryView, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	owner := AgentAuditOwner{
		UserID:         userID,
		ConversationID: conversationID,
	}
	if err := s.checkOwner(ctx, owner); err != nil {
		return nil, err
	}

	messages, err := s.messages.ListByConversationID(ctx, conversationID)
	if err != nil {
		return nil, err
	}

	ids := make([]string, 0)
	seen := make(map[string]bool)
	for _, message := range messages {
		if message.ConversationID != conversationID {
			return nil, ErrAgentAuditUnavailable
		}
		if message.AgentRunID == nil {
			continue
		}

		id := *message.AgentRunID
		if err := ValidateAppealOperationKey(id); err != nil {
			return nil, ErrAgentAuditUnavailable
		}
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}

	summaries := make(map[string]AgentRunSummary, len(ids))
	for start := 0; start < len(ids); start += AgentReadBatchSize {
		end := start + AgentReadBatchSize
		if end > len(ids) {
			end = len(ids)
		}

		batch := ids[start:end]
		runs, err := s.audits.ListRunsOwned(ctx, owner, batch)
		if err != nil {
			return nil, err
		}

		requested := make(map[string]bool, len(batch))
		for _, id := range batch {
			requested[id] = true
		}

		for _, run := range runs {
			if !requested[run.ID] || run.UserID != userID || run.ConversationID != conversationID {
				return nil, ErrAgentAuditUnavailable
			}
			if _, exists := summaries[run.ID]; exists {
				return nil, ErrAgentAuditUnavailable
			}
			summaries[run.ID] = summarizeAgentRun(run)
		}

		for _, id := range batch {
			if _, exists := summaries[id]; !exists {
				return nil, ErrAgentAuditUnavailable
			}
		}
	}

	result := make([]MessageHistoryView, 0, len(messages))
	for _, message := range messages {
		view := MessageHistoryView{Message: message}
		if message.AgentRunID != nil {
			summary := summaries[*message.AgentRunID]
			view.Agent = &summary
		}
		result = append(result, view)
	}

	return result, nil
}

func (s *AgentReadService) RecoverRun(ctx context.Context, userID uint64, conversationID uint64, runID string) (*AgentRecoveryView, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	owner := AgentAuditOwner{
		UserID:         userID,
		ConversationID: conversationID,
	}
	if err := s.checkOwner(ctx, owner); err != nil {
		return nil, err
	}
	if err := ValidateAppealOperationKey(runID); err != nil {
		return nil, err
	}

	observation, err := s.recovery.Recover(ctx, owner, runID)
	if err != nil {
		return nil, err
	}
	if observation == nil || observation.Stored == nil {
		return nil, ErrAgentAuditUnavailable
	}

	run := observation.Stored.Run
	if run.ID != runID || run.UserID != userID || run.ConversationID != conversationID {
		return nil, ErrAgentAuditUnavailable
	}

	view := &AgentRecoveryView{
		Stored:            summarizeAgentRun(run),
		BusinessEffect:    observation.BusinessEffect,
		RecoveryCode:      observation.RecoveryCode,
		MessageSaveStatus: run.MessageSaveStatus,
		Tools:             make([]AgentToolSummary, 0),
		AuditPending:      run.ExecutionStatus == "running" || run.BusinessEffect != observation.BusinessEffect || observation.RecoveryCode != "none",
	}

	for _, call := range observation.Stored.Calls {
		if call.RunID != runID || !AgentAuditTool(call.ToolName) {
			return nil, ErrAgentAuditUnavailable
		}

		pending := call.Status == "started"
		view.AuditPending = view.AuditPending || pending
		view.Tools = append(view.Tools, AgentToolSummary{
			CallID:         call.ID,
			Sequence:       call.Sequence,
			ToolName:       call.ToolName,
			Status:         call.Status,
			ErrorCode:      call.ErrorCode,
			BusinessEffect: call.BusinessEffect,
			AuditPending:   pending,
		})
	}

	if ticket := observation.Ticket; ticket != nil {
		if ticket.TicketID == 0 {
			return nil, ErrAgentAuditUnavailable
		}
		view.Ticket = &AgentCreatedTicketView{
			TicketID:      strconv.FormatUint(ticket.TicketID, 10),
			InitialStatus: ticket.InitialStatus,
			CreatedAt:     ticket.CreatedAt,
			Replayed:      ticket.Replayed,
		}
	}

	receipt, err := s.audits.ReadMessageReceipt(ctx, owner, runID)
	if err != nil {
		view.MessageSaveStatus = "unknown"
		return view, err
	}

	view.Messages = receipt
	if receipt != nil {
		view.MessageSaveStatus = "saved"
	}

	return view, nil
}
