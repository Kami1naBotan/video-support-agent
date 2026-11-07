package llm

import (
	"context"
	"errors"
	"strings"
	"time"
	"video-support-agent/internal/observability"

	"video-support-agent/internal/config"

	einomodel "github.com/cloudwego/eino/components/model"
)

type AgentModels struct {
	Planner einomodel.ToolCallingChatModel
	Answer  Generator
	Timeout time.Duration
}

func NewAgentModels(ctx context.Context, cfg config.LLMConfig, observers ...*observability.LLMObserver) (AgentModels, error) {
	if len(observers) > 1 {
		return AgentModels{}, errors.New(
			"at most one LLM observer is allowed",
		)
	}

	var observer *observability.LLMObserver
	if len(observers) == 1 {
		observer = observers[0]
	}
	timeout, err := time.ParseDuration(
		strings.TrimSpace(cfg.Timeout),
	)
	if err != nil || timeout <= 0 || timeout > 30*time.Second {
		return AgentModels{}, ErrInvalidInput
	}

	plannerHTTP, err := observer.HTTPClient("planner", timeout)
	if err != nil {
		return AgentModels{}, err
	}

	answerHTTP, err := observer.HTTPClient("answer", timeout)
	if err != nil {
		return AgentModels{}, err
	}

	plannerFields := map[string]any{
		"parallel_tool_calls": false,
		"temperature":         0.0,
	}
	answerFields := map[string]any{}

	if strings.TrimSpace(cfg.Model) == "gpt-6-astra" {
		plannerFields["reasoning_effort"] = "low"
		answerFields["reasoning_effort"] = "low"
	}

	planner, err := newChatModel(ctx, cfg, plannerFields, plannerHTTP)
	if err != nil {
		return AgentModels{}, err
	}

	answer, err := newChatModel(ctx, cfg, answerFields, answerHTTP)
	if err != nil {
		return AgentModels{}, err
	}

	return AgentModels{
		Planner: planner,
		Answer:  answer,
		Timeout: timeout,
	}, nil
}
