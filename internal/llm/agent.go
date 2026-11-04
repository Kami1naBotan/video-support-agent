package llm

import (
	"context"
	"strings"
	"time"

	"video-support-agent/internal/config"

	einomodel "github.com/cloudwego/eino/components/model"
)

type AgentModels struct {
	Planner einomodel.ToolCallingChatModel
	Answer  Generator
	Timeout time.Duration
}

func NewAgentModels(ctx context.Context, cfg config.LLMConfig) (AgentModels, error) {
	timeout, err := time.ParseDuration(
		strings.TrimSpace(cfg.Timeout),
	)
	if err != nil || timeout <= 0 || timeout > 30*time.Second {
		return AgentModels{}, ErrInvalidInput
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

	planner, err := newChatModel(ctx, cfg, plannerFields)
	if err != nil {
		return AgentModels{}, err
	}

	answer, err := newChatModel(ctx, cfg, answerFields)
	if err != nil {
		return AgentModels{}, err
	}

	return AgentModels{
		Planner: planner,
		Answer:  answer,
		Timeout: timeout,
	}, nil
}
