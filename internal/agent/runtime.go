package agent

import (
	"context"
	"time"

	"video-support-agent/internal/llm"
	"video-support-agent/internal/service"
	"video-support-agent/internal/tools"
)

type Executor struct {
	*ConfirmedExecutor
	models llm.AgentModels
}

func NewExecutor(deps tools.RegistryDependencies, audit service.AgentAuditStore, recovery service.AgentAppealRecovery, models llm.AgentModels) (*Executor, error) {
	if nilValue(models.Planner) || nilValue(models.Answer) || models.Timeout <= 0 || models.Timeout > 30*time.Second {
		return nil, service.ErrAgentAuditConfiguration
	}

	confirmed, err := NewConfirmedExecutor(deps, audit, recovery)
	if err != nil {
		return nil, err
	}

	return &Executor{
		ConfirmedExecutor: confirmed,
		models:            models,
	}, nil
}

type chatSession struct {
	executor       *Executor
	budget         *runBudget
	owner          service.AgentAuditOwner
	registry       *tools.Registry
	out            *service.AgentOutcome
	sink           service.AgentEventSink
	eventsOK       bool
	stopped        bool
	latestEvidence string
	request        service.AgentRequest
	modelCalls     uint32
}

func (s *chatSession) send(ctx context.Context, event service.AgentEvent) error {
	if !s.eventsOK {
		return nil
	}

	err := emit(ctx, s.sink, event)
	if err != nil {
		s.eventsOK = false
	}
	return err
}

func executionError(code string) error {
	return &service.AgentExecutionError{Code: code}
}

func fallback(out *service.AgentOutcome, decision, code string) {
	out.Decision, out.ReasonCode = decision, code

	out.Content = "目前缺少足够可靠的依据，建议人工核对；本项目尚未接入真人客服。"
	if decision == "clarify" {
		out.Content = "请补充具体事项、相关对象和希望解决的问题，以便继续核对。"
	}
}
