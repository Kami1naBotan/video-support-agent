package service

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"
	"video-support-agent/internal/observability"

	"video-support-agent/internal/cache"
	"video-support-agent/internal/model"

	"go.uber.org/zap"
)

var ErrAgentMessageSave = errors.New("agent message save not confirmed")

type AgentMessageStateStore interface {
	MarkMessageSaveUnknown(context.Context, AgentAuditOwner, string) error
	ReadMessageReceipt(context.Context, AgentAuditOwner, string) (*AgentMessageReceipt, error)
}

type AgentMessageInput struct {
	UserID         uint64             `json:"-"`
	ConversationID uint64             `json:"-"`
	Content        string             `json:"-"`
	Confirmed      *AgentConfirmation `json:"-"`
}

type AgentMessageCallbacks struct {
	OnStart  func() error
	OnEvent  AgentEventSink
	OnResult func(context.Context, *AgentOutcome) error
	OnChunk  func(context.Context, string) error
}

type AgentMessageResult struct {
	UserMessage      *model.Message       `json:"-"`
	AssistantMessage *model.Message       `json:"-"`
	Receipt          *AgentMessageReceipt `json:"-"`
	Outcome          *AgentOutcome        `json:"-"`
}

func NewAgentMessageService(messages MessageStore, conversations ConversationStore, contexts MessageContextStore, logger *zap.Logger, executor AgentExecutor, state AgentMessageStateStore) (*MessageService, error) {
	if messages == nil || conversations == nil || executor == nil || state == nil {
		return nil, ErrAgentAuditConfiguration
	}

	s := NewMessageService(messages, conversations, contexts, logger, nil)
	s.agent = executor
	s.messageState = state
	return s, nil
}

var _ MessageContextStore = (*cache.ContextStore)(nil)

func (s *MessageService) CreateAgent(ctx context.Context, input AgentMessageInput, callbacks AgentMessageCallbacks) (*AgentMessageResult, error) {
	if s == nil || s.agent == nil || s.messageState == nil {
		return nil, ErrAgentAuditConfiguration
	}

	total, cancelTotal := context.WithTimeout(ctx, 40*time.Second)
	defer cancelTotal()

	shared, releaseFinalization := WithAppealFinalization(total)
	defer releaseFinalization()

	work, cancelWork := context.WithCancel(shared)
	defer cancelWork()

	if err := work.Err(); err != nil {
		return nil, err
	}
	if err := s.checkOwnership(work, input.UserID, input.ConversationID); err != nil {
		return nil, err
	}

	request := AgentRequest{
		UserID:         input.UserID,
		ConversationID: input.ConversationID,
	}

	var userContent string
	if input.Confirmed == nil {
		userContent = strings.TrimSpace(input.Content)
		if userContent == "" || !utf8.ValidString(userContent) || utf8.RuneCountInString(userContent) > 10000 {
			return nil, ErrInvalidArgument
		}
		request.Question = userContent
	} else {
		if input.Content != "" {
			return nil, ErrInvalidArgument
		}

		confirmed := *input.Confirmed
		normalized, err := NormalizeCreateAppealInput(confirmed.Input)
		if err != nil {
			return nil, err
		}
		confirmed.Input = normalized
		request.Confirmed = &confirmed

		userContent = "确认提交本项目工单申请。"
	}

	release, acquired := s.guard.TryAcquire(input.ConversationID)
	if !acquired {
		return nil, ErrConversationBusy
	}
	defer release()

	if request.Confirmed == nil {
		history, err := s.loadHistory(work, input.ConversationID)
		if err != nil {
			return nil, err
		}
		request.History = history
	}

	if err := work.Err(); err != nil {
		return nil, err
	}
	if callbacks.OnStart != nil {
		if err := callbacks.OnStart(); err != nil {
			cancelWork()
			return nil, err
		}
	}

	var deliveryErr error
	recordDeliveryError := func(err error) {
		if err != nil && deliveryErr == nil {
			deliveryErr = err
			cancelWork()
		}
	}

	sink := func(eventCtx context.Context, event AgentEvent) error {
		if callbacks.OnEvent == nil {
			return nil
		}
		err := callbacks.OnEvent(eventCtx, event)
		recordDeliveryError(err)
		return err
	}

	out, executeErr := s.agent.Execute(work, request, sink)
	result := &AgentMessageResult{Outcome: out}

	if out == nil {
		return result, errors.Join(executeErr, deliveryErr, ErrAgentAuditUnavailable)
	}

	if out.RunID == "" {
		if executeErr == nil {
			executeErr = ErrAgentAuditUnavailable
		}
		return result, errors.Join(executeErr, deliveryErr)
	}
	if err := ValidateAppealOperationKey(out.RunID); err != nil {
		return result, errors.Join(executeErr, deliveryErr, ErrAgentAuditUnavailable)
	}

	if !utf8.ValidString(out.Content) || strings.TrimSpace(out.Content) == "" {
		return result, errors.Join(executeErr, deliveryErr, ErrAgentAuditUnavailable)
	}

	if deliveryErr == nil && total.Err() == nil && (callbacks.OnResult != nil || callbacks.OnChunk != nil) {
		finish, finishErr := AppealFinalizationContext(shared)
		if finishErr != nil {
			recordDeliveryError(finishErr)
		} else {
			deadline, ok := finish.Deadline()
			if !ok {
				recordDeliveryError(ErrAgentAuditConfiguration)
			} else {
				deliveryCtx, cancelDelivery := context.WithDeadline(work, deadline)

				if err := deliveryCtx.Err(); err != nil {
					recordDeliveryError(err)
				} else {
					if callbacks.OnResult != nil {
						recordDeliveryError(callbacks.OnResult(deliveryCtx, out))
					}
					if deliveryErr == nil && callbacks.OnChunk != nil {
						recordDeliveryError(callbacks.OnChunk(deliveryCtx, out.Content))
					}
				}

				cancelDelivery()
			}
		}
	}

	hasBusinessEffect := out.BusinessEffect == "committed" || out.BusinessEffect == "unknown"

	if !hasBusinessEffect &&
		(executeErr != nil || deliveryErr != nil || total.Err() != nil) {
		return result, errors.Join(executeErr, deliveryErr, total.Err())
	}

	saveErr := s.saveAgentMessages(shared, input, userContent, result)
	observability.RecordAgentMessageSave(shared, out.BusinessEffect, saveErr == nil)

	return result, errors.Join(executeErr, deliveryErr, saveErr, total.Err())
}

func (s *MessageService) saveAgentMessages(ctx context.Context, input AgentMessageInput, userContent string, result *AgentMessageResult) error {
	out := result.Outcome

	finish, err := AppealFinalizationContext(ctx)
	if err != nil || finish.Err() != nil {
		return ErrAgentMessageSave
	}

	owner := AgentAuditOwner{
		UserID:         input.UserID,
		ConversationID: input.ConversationID,
	}

	citations := model.NewMessageCitations(out.Citations)
	if _, err := citations.Value(); err != nil {
		return ErrAgentMessageSave
	}

	out.MessageSaveStatus = "unknown"
	if err := s.messageState.MarkMessageSaveUnknown(finish, owner, out.RunID); err != nil {
		if s.restoreAgentMessageReceipt(finish, owner, result) {
			return nil
		}
		return ErrAgentMessageSave
	}

	runID := out.RunID
	userMessage := &model.Message{
		ConversationID: input.ConversationID,
		Role:           "user",
		Content:        userContent,
		AgentRunID:     &runID,
	}
	assistantMessage := &model.Message{
		ConversationID: input.ConversationID,
		Role:           "assistant",
		Content:        out.Content,
		Citations:      citations,
		AgentRunID:     &runID,
	}

	if err := s.messages.CreatePair(finish, userMessage, assistantMessage); err != nil {
		if s.restoreAgentMessageReceipt(finish, owner, result) {
			return nil
		}
		return ErrAgentMessageSave
	}

	if userMessage.ID == 0 || assistantMessage.ID == 0 {
		if s.restoreAgentMessageReceipt(finish, owner, result) {
			return nil
		}
		return ErrAgentMessageSave
	}

	out.MessageSaveStatus = "saved"
	result.UserMessage = userMessage
	result.AssistantMessage = assistantMessage
	result.Receipt = &AgentMessageReceipt{
		UserMessageID:      userMessage.ID,
		AssistantMessageID: assistantMessage.ID,
	}

	s.refreshContext(finish, input.ConversationID)
	return nil
}

func (s *MessageService) restoreAgentMessageReceipt(ctx context.Context, owner AgentAuditOwner, result *AgentMessageResult) bool {
	receipt, err := s.messageState.ReadMessageReceipt(ctx, owner, result.Outcome.RunID)
	if err != nil || receipt == nil || receipt.UserMessageID == 0 || receipt.AssistantMessageID == 0 {
		return false
	}

	result.Receipt = receipt
	result.Outcome.MessageSaveStatus = "saved"
	return true
}
