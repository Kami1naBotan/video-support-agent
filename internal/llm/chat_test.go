package llm

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

type mockGenerator struct {
	generate func(
		context.Context,
		[]*schema.Message,
		...einomodel.Option,
	) (*schema.Message, error)
}

func (m *mockGenerator) Generate(
	ctx context.Context,
	messages []*schema.Message,
	opts ...einomodel.Option,
) (*schema.Message, error) {
	return m.generate(ctx, messages, opts...)
}

var _ Generator = (*mockGenerator)(nil)

func TestChatClientReplyWithHistory(t *testing.T) {
	calls := 0

	generator := &mockGenerator{
		generate: func(
			ctx context.Context,
			messages []*schema.Message,
			opts ...einomodel.Option,
		) (*schema.Message, error) {
			calls++

			if len(messages) != 4 {
				t.Fatalf("expected 4 messages, got %d", len(messages))
			}

			if messages[0].Role != schema.System ||
				messages[0].Content != supportSystemPrompt {
				t.Fatal("first message must contain the system prompt")
			}

			if messages[1].Role != schema.User ||
				messages[1].Content != "我的投稿被退回了" {
				t.Fatal("second message must be the historical user question")
			}

			if messages[2].Role != schema.Assistant ||
				messages[2].Content != "页面上显示了什么提示？" {
				t.Fatal("third message must be the historical assistant reply")
			}

			if messages[3].Role != schema.User ||
				messages[3].Content != "提示内容不符合要求" {
				t.Fatal("last message must be the current user question")
			}

			if _, ok := ctx.Deadline(); !ok {
				t.Fatal("model call must have a deadline")
			}

			return &schema.Message{
				Role:    schema.Assistant,
				Content: "  请提供更具体的页面提示，以便进一步判断。 \n",
			}, nil
		},
	}

	client, err := NewChatClient(generator, time.Second, nil)
	if err != nil {
		t.Fatalf("create chat client: %v", err)
	}

	history := []HistoryMessage{
		{
			Role:    "user",
			Content: "我的投稿被退回了",
		},
		{
			Role:    "assistant",
			Content: "页面上显示了什么提示？",
		},
	}

	answer, err := client.Reply(
		context.Background(),
		history,
		"  提示内容不符合要求  ",
	)
	if err != nil {
		t.Fatalf("reply failed: %v", err)
	}

	if answer != "请提供更具体的页面提示，以便进一步判断。" {
		t.Fatalf("unexpected answer: %q", answer)
	}

	if calls != 1 {
		t.Fatalf("expected 1 model call, got %d", calls)
	}
}

func TestChatClientReplyRejectsEmptyQuestion(t *testing.T) {
	called := false

	generator := &mockGenerator{
		generate: func(
			ctx context.Context,
			messages []*schema.Message,
			opts ...einomodel.Option,
		) (*schema.Message, error) {
			called = true
			return nil, nil
		},
	}

	client, err := NewChatClient(generator, time.Second, nil)
	if err != nil {
		t.Fatalf("create chat client: %v", err)
	}

	answer, err := client.Reply(context.Background(), nil, " \n\t ")

	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput, got %v", err)
	}
	if answer != "" {
		t.Fatalf("expected empty answer, got %q", answer)
	}
	if called {
		t.Fatal("model must not be called for an empty question")
	}
}

func TestChatClientReplyModelErrors(t *testing.T) {
	tests := []struct {
		name     string
		response *schema.Message
		modelErr error
		wantErr  error
	}{
		{
			name:     "upstream failure",
			modelErr: errors.New("upstream service unavailable"),
			wantErr:  ErrModelFailed,
		},
		{
			name:    "nil response",
			wantErr: ErrEmptyReply,
		},
		{
			name: "empty content",
			response: &schema.Message{
				Role:    schema.Assistant,
				Content: "",
			},
			wantErr: ErrEmptyReply,
		},
		{
			name: "whitespace content",
			response: &schema.Message{
				Role:    schema.Assistant,
				Content: " \n\t ",
			},
			wantErr: ErrEmptyReply,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			generator := &mockGenerator{
				generate: func(
					ctx context.Context,
					messages []*schema.Message,
					opts ...einomodel.Option,
				) (*schema.Message, error) {
					return tt.response, tt.modelErr
				},
			}

			client, err := NewChatClient(generator, time.Second, nil)
			if err != nil {
				t.Fatalf("create chat client: %v", err)
			}

			answer, err := client.Reply(
				context.Background(),
				nil,
				"我的投稿被退回了",
			)

			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("expected %v, got %v", tt.wantErr, err)
			}
			if answer != "" {
				t.Fatalf("expected empty answer, got %q", answer)
			}
		})
	}
}

func TestChatClientReplyTimeout(t *testing.T) {
	generator := &mockGenerator{
		generate: func(
			ctx context.Context,
			messages []*schema.Message,
			opts ...einomodel.Option,
		) (*schema.Message, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}

	client, err := NewChatClient(generator, 20*time.Millisecond, nil)
	if err != nil {
		t.Fatalf("create chat client: %v", err)
	}

	parentCtx, cancel := context.WithTimeout(
		context.Background(),
		2*time.Second,
	)
	defer cancel()

	answer, err := client.Reply(parentCtx, nil, "我的投稿被退回了")

	if !errors.Is(err, ErrModelTimeout) {
		t.Fatalf("expected ErrModelTimeout, got %v", err)
	}
	if answer != "" {
		t.Fatalf("expected empty answer, got %q", answer)
	}
	if parentCtx.Err() != nil {
		t.Fatalf("parent context ended unexpectedly: %v", parentCtx.Err())
	}
}

func TestChatClientReplyCanceled(t *testing.T) {
	parentCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	generator := &mockGenerator{
		generate: func(
			ctx context.Context,
			messages []*schema.Message,
			opts ...einomodel.Option,
		) (*schema.Message, error) {
			cancel()

			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			default:
				t.Fatal("model context must inherit parent cancellation")
				return nil, nil
			}
		},
	}

	client, err := NewChatClient(generator, time.Second, nil)
	if err != nil {
		t.Fatalf("create chat client: %v", err)
	}

	answer, err := client.Reply(parentCtx, nil, "我的投稿被退回了")

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if answer != "" {
		t.Fatalf("expected empty answer, got %q", answer)
	}
}

func TestChatClientReplyTrimsOldestTurn(t *testing.T) {
	calls := 0

	generator := &mockGenerator{
		generate: func(
			ctx context.Context,
			messages []*schema.Message,
			opts ...einomodel.Option,
		) (*schema.Message, error) {
			calls++

			wantRoles := []schema.RoleType{
				schema.System,
				schema.User,
				schema.Assistant,
				schema.User,
			}
			wantContents := []string{
				supportSystemPrompt,
				"最近的问题",
				"最近的回答",
				"继续解释",
			}

			if len(messages) != len(wantContents) {
				t.Fatalf("expected 4 messages, got %d", len(messages))
			}

			totalBytes := 0
			for i, message := range messages {
				if message.Role != wantRoles[i] ||
					message.Content != wantContents[i] {
					t.Fatalf("unexpected message at index %d", i)
				}
				totalBytes += len(message.Content)
			}
			if totalBytes > maxInputTextBytes {
				t.Fatalf("input exceeds budget: %d bytes", totalBytes)
			}

			return &schema.Message{
				Role:    schema.Assistant,
				Content: "继续说明",
			}, nil
		},
	}

	client, err := NewChatClient(generator, time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}

	history := []HistoryMessage{
		{Role: "user", Content: strings.Repeat("旧", maxInputTextBytes/3+1)},
		{Role: "assistant", Content: "这条旧回答也必须移除"},
		{Role: "user", Content: "最近的问题"},
		{Role: "assistant", Content: "最近的回答"},
	}

	answer, err := client.Reply(context.Background(), history, "继续解释")
	if err != nil {
		t.Fatalf("reply failed: %v", err)
	}
	if answer != "继续说明" || calls != 1 {
		t.Fatalf("unexpected result: answer=%q calls=%d", answer, calls)
	}

	if len(history) != 4 ||
		history[0].Content != strings.Repeat("旧", maxInputTextBytes/3+1) ||
		history[1].Content != "这条旧回答也必须移除" {
		t.Fatal("trimming must not modify the original history")
	}
}

func TestChatClientReplyInputBudget(t *testing.T) {
	remaining := maxInputTextBytes - len(supportSystemPrompt)
	if remaining <= 2 {
		t.Fatal("system prompt leaves insufficient space for test input")
	}

	tests := []struct {
		name         string
		question     string
		history      []HistoryMessage
		wantContents []string
		wantErr      error
	}{
		{
			name:     "required input exactly at budget",
			question: strings.Repeat("x", remaining),
			wantContents: []string{
				supportSystemPrompt,
				strings.Repeat("x", remaining),
			},
		},
		{
			name:     "required input exceeds budget by one byte",
			question: strings.Repeat("x", remaining+1),
			wantErr:  ErrInputTooLarge,
		},
		{
			name:     "history exactly fills budget",
			question: "q",
			history: []HistoryMessage{
				{Role: "user", Content: strings.Repeat("h", remaining-2)},
				{Role: "assistant", Content: "a"},
			},
			wantContents: []string{
				supportSystemPrompt,
				strings.Repeat("h", remaining-2),
				"a",
				"q",
			},
		},
		{
			name:     "remove entire history when one byte over budget",
			question: "q",
			history: []HistoryMessage{
				{Role: "user", Content: strings.Repeat("h", remaining-1)},
				{Role: "assistant", Content: "a"},
			},
			wantContents: []string{
				supportSystemPrompt,
				"q",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			generator := &mockGenerator{
				generate: func(
					ctx context.Context,
					messages []*schema.Message,
					opts ...einomodel.Option,
				) (*schema.Message, error) {
					calls++
					if tt.wantErr != nil {
						t.Fatal("oversized input must not call the model")
					}
					if len(messages) != len(tt.wantContents) {
						t.Fatalf(
							"expected %d messages, got %d",
							len(tt.wantContents), len(messages),
						)
					}

					totalBytes := 0
					for i, message := range messages {
						if message.Content != tt.wantContents[i] {
							t.Fatalf("unexpected content at index %d", i)
						}
						totalBytes += len(message.Content)
					}
					if totalBytes > maxInputTextBytes {
						t.Fatalf("input exceeds budget: %d bytes", totalBytes)
					}

					return &schema.Message{
						Role:    schema.Assistant,
						Content: "测试回答",
					}, nil
				},
			}

			client, err := NewChatClient(generator, time.Second, nil)
			if err != nil {
				t.Fatal(err)
			}

			answer, err := client.Reply(
				context.Background(), tt.history, tt.question,
			)

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("expected %v, got %v", tt.wantErr, err)
				}
				if answer != "" || calls != 0 {
					t.Fatal("rejected input must return no answer and make no model call")
				}
				return
			}

			if err != nil {
				t.Fatalf("reply failed: %v", err)
			}
			if answer != "测试回答" || calls != 1 {
				t.Fatalf("unexpected result: answer=%q calls=%d", answer, calls)
			}
		})
	}
}
