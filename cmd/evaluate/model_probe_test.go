//go:build evaluationlive

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/schema"
	"video-support-agent/internal/config"
	"video-support-agent/internal/llm"
)

// Diagnostic only, never counted as an evaluation sample. No provider text or credential is persisted.
func TestEvaluationModelDiagnostic(t *testing.T) {
	if os.Getenv("EVAL_LLM_API_KEY") == "" || os.Getenv("EVALUATION_EVIDENCE") == "" {
		t.Fatal("diagnostic credential and evidence required")
	}
	rows := []map[string]any{}
	for _, base := range []string{"https://api.a6api.vip", "https://api.a6api.vip/v1"} {
		ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
		models, err := llm.NewAgentModels(ctx, config.LLMConfig{BaseURL: base, Model: "gpt-6-astra", APIKey: os.Getenv("EVAL_LLM_API_KEY"), Timeout: "30s"})
		row := map[string]any{"endpoint": base, "model": "gpt-6-astra", "diagnostic_not_quality_sample": true, "success": false}
		if err == nil {
			message, callErr := models.Planner.Generate(ctx, []*schema.Message{schema.SystemMessage("Return only OK."), schema.UserMessage("Diagnostic connectivity check.")})
			err = callErr
			row["success"] = err == nil && message != nil
			if message != nil && message.ResponseMeta != nil {
				row["finish_reason"] = message.ResponseMeta.FinishReason
			}
		}
		if err != nil {
			row["error_type"] = fmt.Sprintf("%T", err)
			if os.Getenv("EVAL_LLM_API_KEY") == "diagnostic-invalid-no-real-credential" {
				row["dummy_credential_diagnostic_error"] = err.Error()
			}
			var api *openai.APIError
			row["cancelled"] = errors.Is(err, context.Canceled)
			row["timed_out"] = errors.Is(err, context.DeadlineExceeded)
			if errors.As(err, &api) {
				row["http_status"] = api.HTTPStatusCode
			}
			msg := strings.ToLower(err.Error())
			for _, word := range []string{"unauthorized", "invalid_api_key", "invalid api key", "quota", "unsupported", "not found", "invalid url", "max_tokens", "temperature", "reasoning_effort", "invalid character"} {
				row["mentions_"+strings.ReplaceAll(word, " ", "_")] = strings.Contains(msg, word)
			}
		}
		cancel()
		rows = append(rows, row)
	}
	liveSave(t, filepath.Join(os.Getenv("EVALUATION_EVIDENCE"), "model-diagnostic.json"), rows)
}
