package agent

import (
	"context"
	"encoding/json"
	"time"
	"unicode/utf8"

	"video-support-agent/internal/model"
	"video-support-agent/internal/service"
	"video-support-agent/internal/tools"
)

func (s *chatSession) step(modelID, name, canonical string) (result string, stop bool, returnErr error) {
	defer func() {
		if stop || returnErr != nil {
			s.stopped = true
		}
	}()

	if s.stopped || s.out.AuditPending {
		return "", true, executionError("protocol_error")
	}
	if err := s.budget.work.Err(); err != nil {
		return "", true, err
	}
	if len(s.out.Tools) >= 5 {
		return "", true, executionError("budget_exhausted")
	}

	started, err := s.executor.audit.StartCall(
		s.budget.work,
		s.owner,
		s.out.RunID,
		service.AgentCallStart{
			ToolName:      name,
			ModelCallID:   modelID,
			ArgumentBytes: uint32(len(canonical)),
		},
	)
	if err != nil {
		s.out.AuditPending = true
		return "", true, err
	}

	if !s.validStartedCall(started, modelID, name, len(canonical)) {
		s.out.AuditPending = true
		return "", true, executionError("invalid_result")
	}

	callStarted := time.Now()

	err = s.send(s.budget.work, service.AgentEvent{
		Kind:           "tool_start",
		RunID:          s.out.RunID,
		CallID:         started.ID,
		Sequence:       started.Sequence,
		ToolName:       name,
		Status:         "started",
		BusinessEffect: "none",
	})

	if err == nil {
		if name == tools.SearchRuleName {
			s.latestEvidence = ""
		}

		invokeCtx := tools.WithEvaluationCall(
			s.budget.work,
			tools.EvaluationCallIdentity{
				RunID:    s.out.RunID,
				CallID:   started.ID,
				Sequence: started.Sequence,
			},
		)
		result, err = s.registry.Invoke(invokeCtx, name, canonical)

		if err == nil {
			switch {
			case len(result) > tools.MaxResultBytes:
				err = executionError("result_too_large")

			case !utf8.ValidString(result) ||
				!json.Valid([]byte(result)):
				err = executionError("invalid_result")

			default:
				stop, err = s.consume(name, result)
			}
		}
	}

	finish := service.AgentCallFinish{
		Status:         "succeeded",
		ErrorCode:      "none",
		BusinessEffect: "none",
	}

	if err != nil {
		stop = true
		finish.Status = callFailure(errorCode(err))
		finish.ErrorCode = errorCode(err)

		if finish.Status == "unknown" {
			err = executionError("invalid_result")
			finish.Status = "failed"
			finish.ErrorCode = "invalid_result"
		}
	} else if name == tools.CreateAppealTicketName {
		finish.Status = "rejected"
		finish.ErrorCode = "confirmation_required"
	}

	auditCtx := s.budget.work
	var auditErr error

	if stop {
		auditCtx, auditErr = service.AppealFinalizationContext(s.budget.total)
	}

	if auditErr == nil {
		auditErr = s.executor.audit.FinishCall(auditCtx, s.owner, s.out.RunID, started.ID, finish)
	}

	if auditErr != nil {
		s.out.AuditPending = true
		stop = true

		if err == nil {
			err = auditErr
		}
	}

	s.out.Tools = append(
		s.out.Tools,
		service.AgentToolSummary{
			CallID:         started.ID,
			Sequence:       started.Sequence,
			ToolName:       name,
			Status:         finish.Status,
			ErrorCode:      finish.ErrorCode,
			BusinessEffect: "none",
			AuditPending:   s.out.AuditPending,
		},
	)

	durationMS := time.Since(callStarted).Milliseconds()

	event := service.AgentEvent{
		Kind:           "tool_end",
		RunID:          s.out.RunID,
		CallID:         started.ID,
		Sequence:       started.Sequence,
		ToolName:       name,
		Status:         finish.Status,
		ErrorCode:      finish.ErrorCode,
		BusinessEffect: "none",
		AuditPending:   s.out.AuditPending,
		DurationMS:     &durationMS,
	}

	if s.out.TicketStatus != nil {
		event.TicketID = s.out.TicketStatus.TicketID
	}

	eventErr := s.sendToolEnd(event, stop)
	if err == nil {
		err = eventErr
	}

	return result, stop || err != nil, err
}

func (s *chatSession) validStartedCall(call *model.ToolCall, modelID, name string, argumentBytes int) bool {
	return call != nil &&
		service.ValidateAppealOperationKey(call.ID) == nil &&
		call.RunID == s.out.RunID &&
		call.Sequence == uint32(len(s.out.Tools)+1) &&
		call.ToolName == name &&
		call.Status == "started" &&
		call.ErrorCode == "none" &&
		call.BusinessEffect == "none" &&
		call.ArgumentBytes == uint32(argumentBytes) &&
		!call.Confirmed &&
		!call.WriteAuthorized &&
		call.ModelCallID != nil &&
		*call.ModelCallID == modelID &&
		call.OperationKey == "" &&
		call.RequestHash == "" &&
		call.PolicyDigest == ""
}

func (s *chatSession) sendToolEnd(event service.AgentEvent, stop bool) error {
	if !s.eventsOK {
		return nil
	}
	if !stop {
		return s.send(s.budget.work, event)
	}

	finish, err := service.AppealFinalizationContext(s.budget.total)
	if err != nil {
		return err
	}

	deadline, _ := finish.Deadline()

	eventCtx, cancel := context.WithDeadline(s.budget.total, deadline)
	defer cancel()

	return s.send(eventCtx, event)
}
