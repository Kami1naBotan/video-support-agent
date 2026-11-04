package agent

import (
	"context"
	"encoding/json"
	"reflect"
	"strconv"
	"time"

	"video-support-agent/internal/model"
	"video-support-agent/internal/service"
	"video-support-agent/internal/tools"
)

type ConfirmedExecutor struct {
	deps     tools.RegistryDependencies
	audit    service.AgentAuditStore
	recovery service.AgentAppealRecovery
}

var _ service.ConfirmedAgentExecutor = (*ConfirmedExecutor)(nil)

func NewConfirmedExecutor(deps tools.RegistryDependencies, audit service.AgentAuditStore, recovery service.AgentAppealRecovery) (*ConfirmedExecutor, error) {
	for _, value := range []any{
		deps.Conversations,
		deps.Retriever,
		deps.Knowledge,
		deps.Appeals,
		audit,
		recovery,
	} {
		if nilValue(value) {
			return nil, service.ErrAgentAuditConfiguration
		}
	}

	deps.WriteAudit = nil

	return &ConfirmedExecutor{
		deps:     deps,
		audit:    audit,
		recovery: recovery,
	}, nil
}

func nilValue(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface,
		reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	}
	return false
}

type createPermit struct {
	store    service.AgentAuditStore
	owner    service.AgentAuditOwner
	runID    string
	callID   string
	expected service.AppealConfirmationBinding
	used     bool
	failed   bool
}

var _ tools.CreateWriteAudit = (*createPermit)(nil)

func (p *createPermit) BeforeCreate(ctx context.Context, binding service.AppealConfirmationBinding) error {
	if p.used || p.runID == "" || p.callID == "" || binding != p.expected {
		p.failed = true
		return service.ErrAgentAuditConflict
	}

	p.used = true
	err := p.store.AuthorizeCreate(ctx, p.owner, p.runID, p.callID, binding)

	p.failed = err != nil
	return err
}

func (e *ConfirmedExecutor) Confirm(ctx context.Context, request service.AgentConfirmedRequest, sink service.AgentEventSink) (out *service.AgentOutcome, returnErr error) {
	out = &service.AgentOutcome{
		ExecutionStatus:   "failed",
		Decision:          "failed",
		ReasonCode:        "none",
		BusinessEffect:    "none",
		MessageSaveStatus: "not_attempted",
		Citations:         []model.RuleCitation{},
		Tools:             []service.AgentToolSummary{},
	}

	defer func() {
		projectOutcome(out, returnErr)
		returnErr = safeError(returnErr)
	}()

	if e == nil {
		return out, service.ErrAgentAuditConfiguration
	}

	budget := newRunBudget(ctx)
	defer budget.close()

	if err := budget.work.Err(); err != nil {
		return out, err
	}

	owner := service.AgentAuditOwner{
		UserID:         request.UserID,
		ConversationID: request.ConversationID,
	}
	caller, err := tools.NewCaller(owner.UserID, owner.ConversationID)
	if err != nil {
		return out, err
	}

	input, err := service.NormalizeCreateAppealInput(request.Input)
	if err != nil {
		return out, err
	}
	hash, err := service.AppealRequestHash(owner.ConversationID, input)
	if err != nil {
		return out, err
	}

	binding := service.AppealConfirmationBinding{
		UserID:         owner.UserID,
		ConversationID: owner.ConversationID,
		IdempotencyKey: request.Confirmation.IdempotencyKey,
		RequestHash:    hash,
		PolicyDigest:   request.Confirmation.PolicyDigest,
	}
	if err := service.ValidateAppealConfirmationBinding(binding); err != nil {
		return out, err
	}

	raw, err := json.Marshal(input)
	if err != nil || len(raw) > tools.MaxArgumentBytes {
		return out, service.ErrInvalidArgument
	}

	permit := &createPermit{
		store:    e.audit,
		owner:    owner,
		expected: binding,
	}
	deps := e.deps
	deps.WriteAudit = permit

	registry, err := tools.NewRegistry(
		caller,
		deps,
		&tools.ConfirmedAppeal{
			Input:        input,
			Confirmation: request.Confirmation,
		},
	)
	if err != nil {
		return out, err
	}

	run, err := e.audit.StartRun(budget.work, owner)
	if err != nil {
		out.AuditPending = true
		return out, err
	}

	if run == nil ||
		service.ValidateAppealOperationKey(run.ID) != nil ||
		run.UserID != owner.UserID ||
		run.ConversationID != owner.ConversationID ||
		run.ExecutionStatus != "running" ||
		run.ToolCalls != 0 ||
		run.ModelCalls != 0 {
		out.AuditPending = true
		return out, &service.AgentExecutionError{
			Code: "invalid_result",
		}
	}

	out.RunID, permit.runID = run.ID, run.ID

	var call *model.ToolCall
	var callStarted time.Time
	eventsOK := true

	send := func(eventCtx context.Context, event service.AgentEvent) error {
		if !eventsOK {
			return nil
		}
		err := emit(eventCtx, sink, event)
		if err != nil {
			eventsOK = false
		}
		return err
	}

	defer func() {
		out.Ticket, out.BusinessEffect = registry.BusinessResult()

		finishCtx, finishErr := service.AppealFinalizationContext(
			budget.total,
		)
		if finishErr != nil {
			out.AuditPending = true
			if returnErr == nil {
				returnErr = finishErr
			}
			return
		}

		if permit.failed {
			out.AuditPending = true
		}

		if call != nil {
			finish := service.AgentCallFinish{
				Status:         callFailure(errorCode(returnErr)),
				ErrorCode:      errorCode(returnErr),
				BusinessEffect: out.BusinessEffect,
			}

			switch out.BusinessEffect {
			case "committed":
				finish.Status = "succeeded"
				finish.ErrorCode = "none"
				finish.Ticket = out.Ticket

			case "unknown":
				finish.Status = "unknown"
				finish.ErrorCode = "outcome_unknown"
				if returnErr == nil {
					returnErr = service.ErrAppealOutcomeUnknown
				}

			default:
				if returnErr == nil {
					returnErr = &service.AgentExecutionError{
						Code: "invalid_result",
					}
					finish.Status = "failed"
					finish.ErrorCode = "invalid_result"
				}
			}

			if !out.AuditPending {
				if err := e.audit.FinishCall(
					finishCtx,
					owner,
					run.ID,
					call.ID,
					finish,
				); err != nil {
					out.AuditPending = true
					if returnErr == nil {
						returnErr = err
					}
				}
			}

			summary := service.AgentToolSummary{
				CallID:         call.ID,
				Sequence:       call.Sequence,
				ToolName:       call.ToolName,
				Status:         finish.Status,
				ErrorCode:      finish.ErrorCode,
				BusinessEffect: out.BusinessEffect,
				AuditPending:   out.AuditPending,
			}
			out.Tools = append(out.Tools, summary)

			durationMS := time.Since(callStarted).Milliseconds()
			event := service.AgentEvent{
				Kind:           "tool_end",
				RunID:          run.ID,
				CallID:         call.ID,
				Sequence:       call.Sequence,
				ToolName:       call.ToolName,
				Status:         finish.Status,
				ErrorCode:      finish.ErrorCode,
				BusinessEffect: out.BusinessEffect,
				AuditPending:   out.AuditPending,
				DurationMS:     &durationMS,
			}
			if out.Ticket != nil {
				event.TicketID = strconv.FormatUint(out.Ticket.TicketID, 10)
				event.Replayed = out.Ticket.Replayed
			}

			eventDeadline, _ := finishCtx.Deadline()
			eventCtx, cancelEvent := context.WithDeadline(
				budget.total, eventDeadline,
			)
			eventErr := send(eventCtx, event)
			cancelEvent()

			if eventErr != nil && returnErr == nil {
				returnErr = eventErr
			}
		}

		projectOutcome(out, returnErr)

		if !out.AuditPending {
			if err := e.audit.FinishRun(
				finishCtx,
				owner,
				run.ID,
				service.AgentRunFinish{
					Status:     out.ExecutionStatus,
					Decision:   out.Decision,
					ReasonCode: out.ReasonCode,
					ModelCalls: 0,
				},
			); err != nil {
				out.AuditPending = true
				if returnErr == nil {
					returnErr = err
				}
			}
		}
	}()

	if err := send(budget.work, service.AgentEvent{
		Kind:  "agent_start",
		RunID: run.ID,
	}); err != nil {
		return out, err
	}

	if err := budget.work.Err(); err != nil {
		return out, err
	}

	started, err := e.audit.StartCall(
		budget.work,
		owner,
		run.ID,
		service.AgentCallStart{
			ToolName:      tools.CreateAppealTicketName,
			ArgumentBytes: uint32(len(raw)),
			Binding:       &binding,
		},
	)
	if err != nil {
		out.AuditPending = true
		return out, err
	}

	if started == nil ||
		service.ValidateAppealOperationKey(started.ID) != nil ||
		started.RunID != run.ID ||
		started.Sequence != 1 ||
		started.Status != "started" ||
		started.ToolName != tools.CreateAppealTicketName ||
		!started.Confirmed ||
		started.WriteAuthorized ||
		started.ModelCallID != nil ||
		started.OperationKey != binding.IdempotencyKey ||
		started.RequestHash != binding.RequestHash ||
		started.PolicyDigest != binding.PolicyDigest {
		out.AuditPending = true
		return out, &service.AgentExecutionError{
			Code: "invalid_result",
		}
	}

	call, permit.callID = started, started.ID
	callStarted = time.Now()

	if err := send(budget.work, service.AgentEvent{
		Kind:           "tool_start",
		RunID:          run.ID,
		CallID:         call.ID,
		Sequence:       call.Sequence,
		ToolName:       call.ToolName,
		Status:         "started",
		BusinessEffect: "none",
	}); err != nil {
		return out, err
	}

	if err := budget.work.Err(); err != nil {
		return out, err
	}

	_, err = registry.InvokeConfirmed(budget.work)

	out.Ticket, out.BusinessEffect = registry.BusinessResult()
	return out, err
}

func (e *ConfirmedExecutor) Recover(ctx context.Context, owner service.AgentAuditOwner, runID string) (*service.AgentAuditObservation, error) {
	if e == nil {
		return nil, service.ErrAgentAuditConfiguration
	}
	if err := owner.Validate(); err != nil {
		return nil, safeError(err)
	}
	if err := service.ValidateAppealOperationKey(runID); err != nil {
		return nil, safeError(err)
	}

	work, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	observed, err := service.ObserveAgentRun(work, e.audit, e.recovery, owner, runID)
	return observed, safeError(err)
}
