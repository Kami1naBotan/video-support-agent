package llm

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"
	"video-support-agent/internal/config"

	"github.com/cloudwego/eino-ext/components/model/openai"
)

const maxOutputTokens = 1024

var _ Generator = (*openai.ChatModel)(nil)

func NewChatModel(ctx context.Context, cfg config.LLMConfig) (*openai.ChatModel, error) {
	baseURL := strings.TrimSpace(cfg.BaseURL)
	modelName := strings.TrimSpace(cfg.Model)
	apiKey := strings.TrimSpace(cfg.APIKey)

	if baseURL == "" {
		return nil, errors.New("LLM_BASE_URL is required")
	}
	if modelName == "" {
		return nil, errors.New("LLM_MODEL is required")
	}
	if apiKey == "" {
		return nil, errors.New("LLM_API_KEY is required")
	}

	parsedURL, err := url.Parse(baseURL)
	if err != nil || parsedURL.Hostname() == "" || (parsedURL.Scheme != "http" && parsedURL.Scheme != "https") {
		return nil, errors.New("LLM_BASE_URL must be a valid HTTP or HTTPS URL")
	}

	if parsedURL.User != nil || parsedURL.RawQuery != "" || parsedURL.Fragment != "" {
		return nil, errors.New("LLM_BASE_URL must not contain credentials, query parameters or a fragment")
	}

	timeout, err := time.ParseDuration(strings.TrimSpace(cfg.Timeout))
	if err != nil || timeout <= 0 {
		return nil, errors.New("LLM_TIMEOUT must be a positive duration, such as 30s")
	}

	outputLimit := maxOutputTokens

	return openai.NewChatModel(ctx, &openai.ChatModelConfig{
		BaseURL:   strings.TrimRight(baseURL, "/"),
		Model:     modelName,
		APIKey:    apiKey,
		Timeout:   timeout,
		MaxTokens: &outputLimit,
	})
}
