package llm

import (
	"context"
	"testing"
	"video-support-agent/internal/config"
)

func TestNewChatModelInvalidConfig(t *testing.T) {
	tests := []struct {
		name    string
		change  func(*config.LLMConfig)
		wantErr string
	}{
		{
			name: "missing base URL",
			change: func(cfg *config.LLMConfig) {
				cfg.BaseURL = " "
			},
			wantErr: "LLM_BASE_URL is required",
		},
		{
			name: "missing model",
			change: func(cfg *config.LLMConfig) {
				cfg.Model = ""
			},
			wantErr: "LLM_MODEL is required",
		},
		{
			name: "missing API key",
			change: func(cfg *config.LLMConfig) {
				cfg.APIKey = " "
			},
			wantErr: "LLM_API_KEY is required",
		},
		{
			name: "invalid URL",
			change: func(cfg *config.LLMConfig) {
				cfg.BaseURL = "not-a-url"
			},
			wantErr: "LLM_BASE_URL must be a valid HTTP or HTTPS URL",
		},
		{
			name: "unsupported URL scheme",
			change: func(cfg *config.LLMConfig) {
				cfg.BaseURL = "ftp://example.com/v1"
			},
			wantErr: "LLM_BASE_URL must be a valid HTTP or HTTPS URL",
		},
		{
			name: "credentials in URL",
			change: func(cfg *config.LLMConfig) {
				cfg.BaseURL = "https://user:password@example.com/v1"
			},
			wantErr: "LLM_BASE_URL must not contain credentials, query parameters or a fragment",
		},
		{
			name: "query parameters in URL",
			change: func(cfg *config.LLMConfig) {
				cfg.BaseURL = "https://example.com/v1?key=test"
			},
			wantErr: "LLM_BASE_URL must not contain credentials, query parameters or a fragment",
		},
		{
			name: "fragment in URL",
			change: func(cfg *config.LLMConfig) {
				cfg.BaseURL = "https://example.com/v1#section"
			},
			wantErr: "LLM_BASE_URL must not contain credentials, query parameters or a fragment",
		},
		{
			name: "invalid timeout",
			change: func(cfg *config.LLMConfig) {
				cfg.Timeout = "abc"
			},
			wantErr: "LLM_TIMEOUT must be a positive duration, such as 30s",
		},
		{
			name: "zero timeout",
			change: func(cfg *config.LLMConfig) {
				cfg.Timeout = "0s"
			},
			wantErr: "LLM_TIMEOUT must be a positive duration, such as 30s",
		},
		{
			name: "negative timeout",
			change: func(cfg *config.LLMConfig) {
				cfg.Timeout = "-1s"
			},
			wantErr: "LLM_TIMEOUT must be a positive duration, such as 30s",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.LLMConfig{
				BaseURL: "https://example.com/v1",
				Model:   "test-model",
				APIKey:  "test-only-key",
				Timeout: "30s",
			}
			tt.change(&cfg)

			client, err := NewChatModel(context.Background(), cfg)

			if err == nil {
				t.Fatal("expected configuration error")
			}
			if client != nil {
				t.Fatal("expected nil client for invalid configuration")
			}
			if err.Error() != tt.wantErr {
				t.Fatalf("unexpected error: got %q, want %q",
					err.Error(), tt.wantErr)
			}
		})
	}
}
