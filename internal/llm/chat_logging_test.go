package llm

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/cloudwego/eino-ext/components/model/openai"
	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestChatClientLogsSafeErrorKind(t *testing.T) {
	core, logs := observer.New(zap.DebugLevel)

	generator := &mockGenerator{
		generate: func(
			_ context.Context,
			_ []*schema.Message,
			_ ...einomodel.Option,
		) (*schema.Message, error) {
			return nil, &openai.APIError{
				HTTPStatusCode: http.StatusUnauthorized,
				Message:        "private-provider-response-marker",
				Code:           "private-provider-code-marker",
			}
		},
	}

	client, err := NewChatClient(
		generator,
		time.Second,
		zap.New(core),
	)
	if err != nil {
		t.Fatal(err)
	}

	answer, err := client.Reply(
		context.Background(),
		nil,
		"private-user-question-marker",
	)

	if !errors.Is(err, ErrModelFailed) {
		t.Fatalf("expected ErrModelFailed, got %v", err)
	}
	if answer != "" {
		t.Fatal("failed model call must not return an answer")
	}

	entries := logs.All()
	if len(entries) != 1 {
		t.Fatalf("expected exactly 1 log entry, got %d", len(entries))
	}

	entry := entries[0]
	if entry.Level != zap.WarnLevel {
		t.Fatalf("expected warning level, got %v", entry.Level)
	}
	if entry.Message != "model call failed" {
		t.Fatal("log message must contain only the fixed description")
	}

	fields := entry.ContextMap()
	if len(fields) != 1 {
		t.Fatal("log must contain only the approved error_kind field")
	}
	if fields["error_kind"] != string(ErrorKindAuthentication) {
		t.Fatal("unexpected error category")
	}
}
