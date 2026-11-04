package agent

import (
	"context"
	"strings"

	"video-support-agent/internal/llm"
	"video-support-agent/internal/model"
	"video-support-agent/internal/service"
	"video-support-agent/internal/tools"

	"github.com/cloudwego/eino/schema"
)

var _ service.AgentExecutor = (*Executor)(nil)

func (e *Executor) Execute(ctx context.Context, request service.AgentRequest, sink service.AgentEventSink) (out *service.AgentOutcome, returnErr error) {
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
		if returnErr != nil &&
			out.RunID == "" &&
			out.Content == "" {
			finishChatOutcome(out, returnErr)
		}
		returnErr = safeError(returnErr)
	}()

	if e == nil || e.ConfirmedExecutor == nil {
		return out, service.ErrAgentAuditConfiguration
	}

	if request.Confirmed != nil {
		if request.Question != "" ||
			len(request.History) != 0 {
			return out, service.ErrInvalidArgument
		}

		return e.Confirm(
			ctx,
			service.AgentConfirmedRequest{
				UserID:         request.UserID,
				ConversationID: request.ConversationID,
				Input:          request.Confirmed.Input,
				Confirmation:   request.Confirmed.Confirmation,
			},
			sink,
		)
	}

	budget := newRunBudget(ctx)
	defer budget.close()

	if err := budget.work.Err(); err != nil {
		return out, err
	}

	messages, err := plannerMessages(request)
	if err != nil {
		return out, err
	}

	request.History = append([]llm.HistoryMessage(nil), request.History...)

	owner := service.AgentAuditOwner{
		UserID:         request.UserID,
		ConversationID: request.ConversationID,
	}

	caller, err := tools.NewCaller(owner.UserID, owner.ConversationID)
	if err != nil {
		return out, err
	}

	registry, err := tools.NewRegistry(caller, e.deps, nil)
	if err != nil {
		return out, err
	}

	infos, err := registry.Infos(budget.work)
	if err != nil {
		return out, err
	}

	planner, err := e.models.Planner.WithTools(infos)
	if err != nil || nilValue(planner) {
		return out, executionError("configuration_error")
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
		run.ModelCalls != 0 ||
		run.BusinessEffect != "none" ||
		run.Decision != "" ||
		run.ReasonCode != "none" ||
		run.MessageSaveStatus != "not_attempted" {
		out.AuditPending = true
		return out, executionError("invalid_result")
	}

	out.RunID = run.ID

	s := &chatSession{
		executor: e,
		budget:   budget,
		owner:    owner,
		request:  request,
		registry: registry,
		out:      out,
		sink:     sink,
		eventsOK: true,
	}

	defer func() {
		if returnErr == nil && budget.work.Err() != nil {
			returnErr = budget.work.Err()
		}

		finishChatOutcome(out, returnErr)

		if !out.AuditPending {
			finish, err := service.AppealFinalizationContext(budget.total)

			if err == nil {
				err = e.audit.FinishRun(
					finish,
					owner,
					run.ID,
					service.AgentRunFinish{
						Status:     out.ExecutionStatus,
						Decision:   out.Decision,
						ReasonCode: out.ReasonCode,
						ModelCalls: s.modelCalls,
					},
				)
			}

			if err != nil {
				out.AuditPending = true
				if returnErr == nil {
					returnErr = err
				}
				finishChatOutcome(out, returnErr)
			}
		}
	}()

	if err := s.send(
		budget.work,
		service.AgentEvent{
			Kind:  "agent_start",
			RunID: run.ID,
		},
	); err != nil {
		return out, err
	}

	seenIDs := map[string]bool{}
	seenCalls := map[string]bool{}

	for {
		if s.modelCalls >= 6 {
			fallback(out, "handoff", "budget_exhausted")
			return out, nil
		}

		raw, err := s.generate(budget.work, planner, messages)
		if err != nil {
			return out, err
		}

		if err := validatePlanMessage(raw); err != nil {
			return out, err
		}

		if len(raw.ToolCalls) == 0 {
			decision, err := decodePlan(raw.Content)
			if err != nil {
				return out, err
			}

			switch decision.Decision {
			case "clarify":
				fallback(out, "clarify", "needs_more_info")

			case "handoff":
				fallback(out, "handoff", "manual_review")

			case "answer_rules":
				if s.latestEvidence == "" || *decision.EvidenceHandle != s.latestEvidence {
					return out, executionError("protocol_error")
				}

				if err := s.answer(); err != nil {
					return out, err
				}
			}

			return out, nil
		}

		call := raw.ToolCalls[0]

		if seenIDs[call.ID] {
			return out, executionError("protocol_error")
		}
		seenIDs[call.ID] = true

		canonical, err := tools.NormalizeCall(call.Function.Name, call.Function.Arguments)
		if err != nil {
			return out, err
		}

		key := call.Function.Name + "\x00" + canonical
		if seenCalls[key] {
			fallback(out, "handoff", "repeated_call")
			return out, nil
		}
		seenCalls[key] = true

		if len(out.Tools) >= 5 {
			fallback(out, "handoff", "budget_exhausted")
			return out, nil
		}

		cleanCall := schema.ToolCall{
			ID:       call.ID,
			Type:     "function",
			Function: call.Function,
		}

		assistant := schema.AssistantMessage("", []schema.ToolCall{cleanCall})

		candidate := append(append([]*schema.Message(nil), messages...), assistant)

		if !messagesFit(candidate) {
			fallback(out, "clarify", "budget_exhausted")
			return out, nil
		}

		result, stop, err := s.step(call.ID, call.Function.Name, canonical)
		if err != nil {
			return out, err
		}
		if stop {
			return out, nil
		}

		messages = append(candidate, schema.ToolMessage(result, call.ID))

		if !messagesFit(messages) {
			fallback(out, "clarify", "budget_exhausted")
			return out, nil
		}
	}
}

func finishChatOutcome(out *service.AgentOutcome, err error) {
	out.ExecutionStatus = "completed"

	if err != nil {
		out.ExecutionStatus = "failed"
		code := errorCode(err)

		if code == "cancelled" || code == "timed_out" {
			out.ExecutionStatus = code
		}
		out.ReasonCode = code

		if out.Proposal == nil && out.TicketStatus == nil {
			out.Decision = "failed"
			out.Content = "本次处理未完成，请稍后重试或人工核对。"
			out.Citations = []model.RuleCitation{}
		}
	}

	const pending = " 本次审计记录尚待核对。"

	out.Content = strings.TrimSuffix(out.Content, pending)
	if out.AuditPending {
		out.Content += pending
	}
}
