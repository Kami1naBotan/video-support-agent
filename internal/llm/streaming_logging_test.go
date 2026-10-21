package llm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/cloudwego/eino-ext/components/model/openai"
	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func TestReplyStreamFinishedLog(t *testing.T) {
	tests := []struct {
		name         string
		content      string
		finishReason string
		cancelParent bool
		callbackErr  error
		wantErr      error
		wantOutcome  string
		wantLevel    zapcore.Level
	}{
		{
			name:         "success after deferred cleanup",
			content:      "private-answer-marker",
			finishReason: "stop",
			wantOutcome:  "success",
			wantLevel:    zap.InfoLevel,
		},
		{
			name:         "parent cancellation",
			cancelParent: true,
			wantErr:      context.Canceled,
			wantOutcome:  "canceled",
			wantLevel:    zap.InfoLevel,
		},
		{
			name:         "incomplete answer",
			content:      "private-partial-answer-marker",
			finishReason: "length",
			wantErr:      ErrIncompleteReply,
			wantOutcome:  "incomplete",
			wantLevel:    zap.WarnLevel,
		},
		{
			name:         "empty answer",
			content:      " \n",
			finishReason: "stop",
			wantErr:      ErrEmptyReply,
			wantOutcome:  "empty_reply",
			wantLevel:    zap.WarnLevel,
		},
		{
			name:         "callback deadline",
			content:      "private-answer-marker",
			finishReason: "stop",
			callbackErr: fmt.Errorf(
				"private-callback-marker: %w",
				context.DeadlineExceeded,
			),
			wantErr:     context.DeadlineExceeded,
			wantOutcome: "timeout",
			wantLevel:   zap.WarnLevel,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			core, logs := observer.New(zap.DebugLevel)

			model := &streamModelStub{
				chunks: []*schema.Message{
					{
						Content: tt.content,
						ResponseMeta: &schema.ResponseMeta{
							FinishReason: tt.finishReason,
						},
					},
				},
			}

			client, err := NewChatClient(
				model,
				time.Second,
				zap.New(core),
			)
			if err != nil {
				t.Fatal(err)
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			if tt.cancelParent {
				cancel()
			}

			answer, err := client.ReplyStream(
				ctx,
				nil,
				"private-question-marker",
				func(string) error {
					return tt.callbackErr
				},
			)

			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("expected error %v, got %v", tt.wantErr, err)
			}
			if tt.wantErr == nil && answer != tt.content {
				t.Fatal("successful answer changed")
			}
			if tt.wantErr != nil && answer != "" {
				t.Fatal("failed call returned an answer")
			}

			entries := logs.All()
			if len(entries) != 1 {
				t.Fatalf("expected one log entry, got %d", len(entries))
			}

			entry := entries[0]
			if entry.Message != "model stream finished" {
				t.Fatal("unexpected log message")
			}
			if entry.Level != tt.wantLevel {
				t.Fatalf("expected level %v, got %v", tt.wantLevel, entry.Level)
			}

			fields := entry.ContextMap()
			if len(fields) != 2 {
				t.Fatal("log must contain only outcome and duration")
			}
			if fields["outcome"] != tt.wantOutcome {
				t.Fatalf("unexpected outcome: %v", fields["outcome"])
			}

			duration, ok := fields["duration"].(time.Duration)
			if !ok || duration < 0 {
				t.Fatal("duration must be a non-negative time.Duration")
			}
		})
	}
}

func TestReplyStreamUpstreamFailureLogs(t *testing.T) {
	tests := []struct {
		name          string
		waitTimeout   bool
		wantErr       error
		wantOutcome   string
		wantDiagnosis bool
	}{
		{
			name:          "upstream authentication failure",
			wantErr:       ErrModelFailed,
			wantOutcome:   "model_failed",
			wantDiagnosis: true,
		},
		{
			name:        "model context timeout",
			waitTimeout: true,
			wantErr:     ErrModelTimeout,
			wantOutcome: "timeout",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			core, logs := observer.New(zap.DebugLevel)

			stub := &streamFuncStub{
				stream: func(
					ctx context.Context,
					_ []*schema.Message,
					_ ...einomodel.Option,
				) (*schema.StreamReader[*schema.Message], error) {
					if tt.waitTimeout {
						// 等待真正的模型 Context 截止时间触发。
						<-ctx.Done()
						return nil, ctx.Err()
					}

					return nil, &openai.APIError{
						HTTPStatusCode: http.StatusUnauthorized,
						Message:        "private-provider-response-marker",
						Code:           "private-provider-code-marker",
					}
				},
			}

			client, err := NewChatClient(
				stub,
				100*time.Millisecond,
				zap.New(core),
			)
			if err != nil {
				t.Fatal(err)
			}

			answer, err := client.ReplyStream(
				context.Background(),
				nil,
				"private-question-marker",
				func(string) error {
					t.Fatal("failed stream initialization must not emit content")
					return nil
				},
			)

			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("expected error %v, got %v", tt.wantErr, err)
			}
			if answer != "" {
				t.Fatal("failed call must not return an answer")
			}

			finished := logs.FilterMessage("model stream finished").All()
			if len(finished) != 1 {
				t.Fatalf("expected one finish log, got %d", len(finished))
			}
			if finished[0].Level != zap.WarnLevel {
				t.Fatal("failed stream must finish with warning level")
			}

			fields := finished[0].ContextMap()
			if len(fields) != 2 {
				t.Fatal("finish log must contain only outcome and duration")
			}
			if fields["outcome"] != tt.wantOutcome {
				t.Fatalf("unexpected outcome: %v", fields["outcome"])
			}

			duration, ok := fields["duration"].(time.Duration)
			if !ok || duration < 0 {
				t.Fatal("invalid duration")
			}

			diagnosis := logs.FilterMessage("model stream failed").All()
			wantDiagnosisCount := 0
			if tt.wantDiagnosis {
				wantDiagnosisCount = 1
			}

			if len(diagnosis) != wantDiagnosisCount {
				t.Fatalf(
					"expected %d diagnosis logs, got %d",
					wantDiagnosisCount,
					len(diagnosis),
				)
			}

			if tt.wantDiagnosis {
				if diagnosis[0].Level != zap.WarnLevel {
					t.Fatal("diagnosis must use warning level")
				}

				details := diagnosis[0].ContextMap()
				if len(details) != 1 ||
					details["error_kind"] != string(ErrorKindAuthentication) {
					t.Fatal("diagnosis must contain only the authentication category")
				}
			}

			if logs.Len() != 1+wantDiagnosisCount {
				t.Fatal("unexpected additional log entries")
			}
		})
	}
}
