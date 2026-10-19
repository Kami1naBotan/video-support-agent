package service

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
	"video-support-agent/internal/llm"
	"video-support-agent/internal/model"
)

type blockingChatResponder struct {
	started chan struct{}
	proceed chan struct{}
	calls   atomic.Int32
}

func (f *blockingChatResponder) Reply(
	ctx context.Context,
	_ []llm.HistoryMessage,
	_ string,
) (string, error) {
	if f.calls.Add(1) != 1 {
		return "", errors.New("unexpected concurrent model call")
	}

	close(f.started)

	select {
	case <-f.proceed:
		return "第一条请求的回答", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func TestMessageServiceRejectsConcurrentSameConversation(t *testing.T) {
	messages := &fakeMessageStore{}
	conversations := &fakeConversationStore{
		found: &model.Conversation{ID: 1, UserID: 7},
	}
	chat := &blockingChatResponder{
		started: make(chan struct{}),
		proceed: make(chan struct{}),
	}

	svc := NewMessageService(
		messages,
		conversations,
		nil,
		nil,
		chat,
	)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)

	type createResult struct {
		message *model.Message
		err     error
	}

	resultCh := make(chan createResult, 1)
	exited := make(chan struct{})

	go func() {
		defer close(exited)

		message, err := svc.Create(ctx, 7, 1, "第一条问题")
		resultCh <- createResult{
			message: message,
			err:     err,
		}
	}()

	defer func() {
		cancel()
		<-exited
	}()

	// 确保第一条请求已经占用会话并进入模型。
	select {
	case <-chat.started:
	case result := <-resultCh:
		t.Fatalf("first request exited before model call: %v", result.err)
	case <-ctx.Done():
		t.Fatalf("waiting for model start: %v", ctx.Err())
	}

	// 第一条请求仍被模型替身阻塞，此时发送第二条请求。
	second, err := svc.Create(ctx, 7, 1, "第二条问题")

	if !errors.Is(err, ErrConversationBusy) {
		t.Fatalf("expected ErrConversationBusy, got %v", err)
	}
	if second != nil {
		t.Fatal("busy request must not return a saved message")
	}
	if chat.calls.Load() != 1 {
		t.Fatalf("expected 1 model call, got %d", chat.calls.Load())
	}

	// 允许第一条请求完成生成、保存及释放。
	close(chat.proceed)

	var first createResult
	select {
	case first = <-resultCh:
	case <-ctx.Done():
		t.Fatalf("waiting for first request: %v", ctx.Err())
	}

	if first.err != nil {
		t.Fatalf("first request failed: %v", first.err)
	}
	if first.message == nil {
		t.Fatal("first request must return the saved user message")
	}

	// 完成信号到达后再检查普通 Fake 字段，避免并发读写。
	if messages.saveCalls != 1 {
		t.Fatalf("expected 1 save attempt, got %d", messages.saveCalls)
	}
	if messages.created == nil ||
		messages.created.Content != "第一条问题" {
		t.Fatal("only the first question should be saved")
	}
	if messages.reply == nil ||
		messages.reply.Content != "第一条请求的回答" {
		t.Fatal("the first model reply should be saved")
	}

	release, acquired := svc.guard.TryAcquire(1)
	if !acquired {
		t.Fatal("conversation must be released after Create returns")
	}
	release()
}

func TestMessageServiceReleasesConversationOnFailure(t *testing.T) {
	saveErr := errors.New("database write failed")

	tests := []struct {
		name          string
		modelErr      error
		saveErr       error
		wantErr       error
		wantSaveCalls int
	}{
		{
			name:     "model failure",
			modelErr: llm.ErrModelFailed,
			wantErr:  llm.ErrModelFailed,
		},
		{
			name:     "model timeout",
			modelErr: llm.ErrModelTimeout,
			wantErr:  llm.ErrModelTimeout,
		},
		{
			name:     "model cancellation",
			modelErr: context.Canceled,
			wantErr:  context.Canceled,
		},
		{
			name:          "database save failure",
			saveErr:       saveErr,
			wantErr:       saveErr,
			wantSaveCalls: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			messages := &fakeMessageStore{
				saveErr: tt.saveErr,
			}
			conversations := &fakeConversationStore{
				found: &model.Conversation{ID: 1, UserID: 7},
			}
			contexts := &messageContextStub{}
			chat := &recordingChatResponder{
				answer: "模型生成的回答",
				err:    tt.modelErr,
			}

			svc := NewMessageService(
				messages,
				conversations,
				contexts,
				nil,
				chat,
			)

			got, err := svc.Create(
				context.Background(),
				7,
				1,
				"我的投稿被退回了",
			)

			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("expected %v, got %v", tt.wantErr, err)
			}
			if got != nil {
				t.Fatal("failed request must not return a saved message")
			}
			if chat.calls != 1 {
				t.Fatalf("expected 1 model call, got %d", chat.calls)
			}
			if messages.saveCalls != tt.wantSaveCalls {
				t.Fatalf(
					"expected %d save attempts, got %d",
					tt.wantSaveCalls,
					messages.saveCalls,
				)
			}
			if contexts.called {
				t.Fatal("failed request must not update the cache")
			}

			release, acquired := svc.guard.TryAcquire(1)
			if !acquired {
				t.Fatal("failed request must release the conversation")
			}
			release()
		})
	}
}
