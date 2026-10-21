package llm

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

type streamModelStub struct {
	chunks []*schema.Message
}

type streamFuncStub struct {
	streamModelStub

	stream func(
		context.Context,
		[]*schema.Message,
		...einomodel.Option,
	) (*schema.StreamReader[*schema.Message], error)
}

func (s *streamFuncStub) Stream(
	ctx context.Context,
	messages []*schema.Message,
	opts ...einomodel.Option,
) (*schema.StreamReader[*schema.Message], error) {
	return s.stream(ctx, messages, opts...)
}

var _ Generator = (*streamFuncStub)(nil)
var _ Streamer = (*streamFuncStub)(nil)

func (s *streamModelStub) Generate(
	ctx context.Context,
	messages []*schema.Message,
	opts ...einomodel.Option,
) (*schema.Message, error) {
	return nil, errors.New("unexpected Generate call")
}

func (s *streamModelStub) Stream(
	ctx context.Context,
	messages []*schema.Message,
	opts ...einomodel.Option,
) (*schema.StreamReader[*schema.Message], error) {
	reader, writer := schema.Pipe[*schema.Message](len(s.chunks))

	for _, chunk := range s.chunks {
		writer.Send(chunk, nil)
	}
	writer.Close()

	return reader, nil
}

var _ Generator = (*streamModelStub)(nil)
var _ Streamer = (*streamModelStub)(nil)

func TestChatClientReplyStreamResults(t *testing.T) {
	tests := []struct {
		name         string
		parts        []string
		finishReason string
		wantAnswer   string
		wantErr      error
	}{
		{
			name:         "preserve chunk order and whitespace",
			parts:        []string{"你好", " world", "\n下一段"},
			finishReason: "stop",
			wantAnswer:   "你好 world\n下一段",
		},
		{
			name:         "reject truncated reply",
			parts:        []string{"尚未完成的回答"},
			finishReason: "length",
			wantErr:      ErrIncompleteReply,
		},
		{
			name:    "reject missing finish reason",
			parts:   []string{"不能只根据 EOF 判断成功"},
			wantErr: ErrIncompleteReply,
		},
		{
			name:         "reject whitespace-only reply",
			parts:        []string{" ", "\n"},
			finishReason: "stop",
			wantErr:      ErrEmptyReply,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chunks := make([]*schema.Message, 0, len(tt.parts)+1)

			for _, part := range tt.parts {
				chunks = append(chunks, &schema.Message{
					Role:    schema.Assistant,
					Content: part,
				})
			}

			// 最后一个片段没有正文，只携带结束原因。
			chunks = append(chunks, &schema.Message{
				Role: schema.Assistant,
				ResponseMeta: &schema.ResponseMeta{
					FinishReason: tt.finishReason,
				},
			})

			client, err := NewChatClient(
				&streamModelStub{chunks: chunks},
				time.Second,
				nil,
			)
			if err != nil {
				t.Fatalf("create client: %v", err)
			}

			var received []string
			answer, err := client.ReplyStream(
				context.Background(),
				nil,
				"你好",
				func(part string) error {
					received = append(received, part)
					return nil
				},
			)

			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("expected error %v, got %v", tt.wantErr, err)
			}

			if answer != tt.wantAnswer {
				t.Fatalf("expected answer %q, got %q", tt.wantAnswer, answer)
			}

			if !reflect.DeepEqual(received, tt.parts) {
				t.Fatalf("expected chunks %#v, got %#v", tt.parts, received)
			}
		})
	}
}

func TestChatClientReplyStreamCallbackFailure(t *testing.T) {
	writeErr := errors.New("client write failed")

	reader, writer := schema.Pipe[*schema.Message](2)
	defer writer.Close()

	writer.Send(&schema.Message{
		Content: "第一段",
	}, nil)
	writer.Send(&schema.Message{
		Content: "不应该继续发送的第二段",
	}, nil)

	var modelCtx context.Context

	stub := &streamFuncStub{
		stream: func(
			ctx context.Context,
			messages []*schema.Message,
			opts ...einomodel.Option,
		) (*schema.StreamReader[*schema.Message], error) {
			modelCtx = ctx
			return reader, nil
		},
	}

	client, err := NewChatClient(stub, time.Minute, nil)
	if err != nil {
		t.Fatalf("create client: %v", err)
	}

	var received []string

	answer, err := client.ReplyStream(
		context.Background(),
		nil,
		"你好",
		func(part string) error {
			received = append(received, part)
			return writeErr
		},
	)

	if !errors.Is(err, writeErr) {
		t.Fatalf("expected callback error, got %v", err)
	}

	if answer != "" {
		t.Fatalf("failed stream must not return an answer, got %q", answer)
	}

	if !reflect.DeepEqual(received, []string{"第一段"}) {
		t.Fatalf("unexpected chunks after callback failure: %#v", received)
	}

	if modelCtx == nil {
		t.Fatal("model Stream was not called")
	}

	if !errors.Is(modelCtx.Err(), context.Canceled) {
		t.Fatalf("model context must be canceled, got %v", modelCtx.Err())
	}

	if closed := writer.Send(&schema.Message{
		Content: "检查读取端是否关闭",
	}, nil); !closed {
		t.Fatal("stream reader must be closed after callback failure")
	}
}

func TestChatClientReplyStreamCancellation(t *testing.T) {
	tests := []struct {
		name         string
		modelTimeout time.Duration
		cancelParent bool
		wantErr      error
	}{
		{
			name:         "parent cancellation",
			modelTimeout: time.Minute,
			cancelParent: true,
			wantErr:      context.Canceled,
		},
		{
			name:         "model timeout",
			modelTimeout: 100 * time.Millisecond,
			wantErr:      ErrModelTimeout,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parentCtx, cancelParent := context.WithCancel(
				context.Background(),
			)
			defer cancelParent()

			started := make(chan struct{})
			upstreamDone := make(chan struct{})

			stub := &streamFuncStub{
				stream: func(
					ctx context.Context,
					messages []*schema.Message,
					opts ...einomodel.Option,
				) (*schema.StreamReader[*schema.Message], error) {
					reader, writer := schema.Pipe[*schema.Message](0)

					go func() {
						defer close(upstreamDone)
						defer writer.Close()

						// 模拟模型没有输出，直到调用被取消。
						<-ctx.Done()
					}()

					close(started)
					return reader, nil
				},
			}

			client, err := NewChatClient(
				stub,
				tt.modelTimeout,
				nil,
			)
			if err != nil {
				t.Fatalf("create client: %v", err)
			}

			type streamResult struct {
				answer        string
				err           error
				callbackCalls int
			}

			resultCh := make(chan streamResult, 1)

			go func() {
				calls := 0

				answer, err := client.ReplyStream(
					parentCtx,
					nil,
					"你好",
					func(part string) error {
						calls++
						return nil
					},
				)

				resultCh <- streamResult{
					answer:        answer,
					err:           err,
					callbackCalls: calls,
				}
			}()

			select {
			case <-started:
				// 确认模型流已开始建立，再执行取消。
			case <-time.After(2 * time.Second):
				t.Fatal("model stream did not start")
			}

			if tt.cancelParent {
				cancelParent()
			}

			var result streamResult

			select {
			case result = <-resultCh:
			case <-time.After(2 * time.Second):
				t.Fatal("ReplyStream did not stop after cancellation or timeout")
			}

			// 不仅检查方法返回，也检查模拟上游已经退出。
			select {
			case <-upstreamDone:
			case <-time.After(2 * time.Second):
				t.Fatal("upstream goroutine did not exit")
			}

			if !errors.Is(result.err, tt.wantErr) {
				t.Fatalf(
					"expected error %v, got %v",
					tt.wantErr,
					result.err,
				)
			}

			if result.answer != "" {
				t.Fatalf("expected no answer, got %q", result.answer)
			}

			if result.callbackCalls != 0 {
				t.Fatalf(
					"expected no callbacks, got %d",
					result.callbackCalls,
				)
			}
		})
	}
}
