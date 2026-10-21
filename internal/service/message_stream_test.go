package service

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
	"video-support-agent/internal/llm"
	"video-support-agent/internal/model"
)

type streamChatStub struct {
	fakeChatResponder

	replyStream func(
		context.Context,
		[]llm.HistoryMessage,
		string,
		func(string) error,
	) (string, error)
}

type streamSaveStore struct {
	fakeMessageStore

	onSave func(context.Context) error
}

func (s *streamSaveStore) CreatePair(
	ctx context.Context,
	userMessage *model.Message,
	assistantMessage *model.Message,
) error {
	if s.onSave != nil {
		if err := s.onSave(ctx); err != nil {
			return err
		}
	}

	return s.fakeMessageStore.CreatePair(
		ctx,
		userMessage,
		assistantMessage,
	)
}

var _ MessageStore = (*streamSaveStore)(nil)

func (s *streamChatStub) ReplyStream(
	ctx context.Context,
	history []llm.HistoryMessage,
	question string,
	onChunk func(string) error,
) (string, error) {
	return s.replyStream(ctx, history, question, onChunk)
}

var _ ChatResponder = (*streamChatStub)(nil)
var _ ChatStreamResponder = (*streamChatStub)(nil)

func TestMessageServiceCreateStreamSuccess(t *testing.T) {
	messages := &fakeMessageStore{}
	conversations := &fakeConversationStore{
		found: &model.Conversation{ID: 1, UserID: 7},
	}
	contexts := &messageContextStub{}

	var events []string
	modelCalls := 0
	wantAnswer := "  第一段\n第二段  "

	chat := &streamChatStub{
		replyStream: func(
			ctx context.Context,
			history []llm.HistoryMessage,
			question string,
			onChunk func(string) error,
		) (string, error) {
			modelCalls++
			events = append(events, "model")

			if question != "我的问题" {
				t.Fatalf("unexpected question: %q", question)
			}

			for _, part := range []string{"  第一段", "\n第二段  "} {
				if err := onChunk(part); err != nil {
					return "", err
				}
			}

			if messages.saveCalls != 0 {
				t.Fatal("must not save before generation completes")
			}

			return wantAnswer, nil
		},
	}

	svc := NewMessageService(
		messages, conversations, contexts, nil, chat,
	)

	result, err := svc.CreateStream(
		context.Background(),
		7,
		1,
		"  我的问题  ",
		StreamCallbacks{
			OnStart: func() error {
				events = append(events, "start")
				return nil
			},
			OnChunk: func(part string) error {
				if messages.saveCalls != 0 {
					t.Fatal("must not save while delivering chunks")
				}
				events = append(events, "chunk:"+part)
				return nil
			},
		},
	)
	if err != nil {
		t.Fatalf("create stream: %v", err)
	}

	wantEvents := []string{
		"start",
		"model",
		"chunk:  第一段",
		"chunk:\n第二段  ",
	}
	if !reflect.DeepEqual(events, wantEvents) {
		t.Fatalf("unexpected event order: %#v", events)
	}

	if modelCalls != 1 || messages.saveCalls != 1 {
		t.Fatalf(
			"expected one model call and one save, got %d and %d",
			modelCalls, messages.saveCalls,
		)
	}

	if result == nil ||
		result.UserMessage == nil ||
		result.AssistantMessage == nil {
		t.Fatal("expected both saved messages")
	}

	if result.UserMessage.ID != 10 ||
		result.AssistantMessage.ID != 11 {
		t.Fatal("result must contain saved message IDs")
	}

	if messages.created == nil || messages.reply == nil {
		t.Fatal("both messages must be passed to the store")
	}

	if messages.created.Role != "user" ||
		messages.created.Content != "我的问题" ||
		messages.created.ConversationID != 1 {
		t.Fatalf("unexpected saved question: %+v", messages.created)
	}

	if messages.reply.Role != "assistant" ||
		messages.reply.Content != wantAnswer ||
		messages.reply.ConversationID != 1 {
		t.Fatalf("unexpected saved answer: %+v", messages.reply)
	}

	if !contexts.called || len(contexts.received) != 2 {
		t.Fatal("cache must be refreshed with the saved pair")
	}

	if contexts.received[0].ID != 10 ||
		contexts.received[1].ID != 11 ||
		contexts.received[1].Content != wantAnswer {
		t.Fatalf("unexpected cache snapshot: %+v", contexts.received)
	}
}

func TestMessageServiceCreateStreamModelFailureDoesNotSave(t *testing.T) {
	messages := &fakeMessageStore{}
	conversations := &fakeConversationStore{
		found: &model.Conversation{ID: 1, UserID: 7},
	}
	contexts := &messageContextStub{}

	chat := &streamChatStub{
		replyStream: func(
			ctx context.Context,
			history []llm.HistoryMessage,
			question string,
			onChunk func(string) error,
		) (string, error) {
			if err := onChunk("尚未完成的片段"); err != nil {
				return "", err
			}
			return "", llm.ErrModelTimeout
		},
	}

	svc := NewMessageService(
		messages, conversations, contexts, nil, chat,
	)

	var received []string
	result, err := svc.CreateStream(
		context.Background(),
		7,
		1,
		"我的问题",
		StreamCallbacks{
			OnStart: func() error {
				return nil
			},
			OnChunk: func(part string) error {
				received = append(received, part)
				return nil
			},
		},
	)

	if !errors.Is(err, llm.ErrModelTimeout) {
		t.Fatalf("expected model timeout, got %v", err)
	}
	if result != nil {
		t.Fatal("failed generation must not return saved messages")
	}

	if !reflect.DeepEqual(received, []string{"尚未完成的片段"}) {
		t.Fatalf("unexpected received chunks: %#v", received)
	}

	if messages.saveCalls != 0 ||
		messages.created != nil ||
		messages.reply != nil {
		t.Fatal("failed generation must not save either message")
	}

	if contexts.called {
		t.Fatal("failed generation must not replace the cache")
	}
}

func TestMessageServiceCreateStreamSaveAfterGenerationCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var observedSaveCtx context.Context

	messages := &streamSaveStore{
		onSave: func(saveCtx context.Context) error {
			observedSaveCtx = saveCtx

			if !errors.Is(ctx.Err(), context.Canceled) {
				t.Fatal("parent request must already be canceled")
			}

			if err := saveCtx.Err(); err != nil {
				t.Fatalf("save context must remain usable: %v", err)
			}

			deadline, ok := saveCtx.Deadline()
			if !ok {
				t.Fatal("save context must have a deadline")
			}

			remaining := time.Until(deadline)
			if remaining <= 0 || remaining > streamSaveTimeout {
				t.Fatalf("unexpected save time budget: %v", remaining)
			}

			return nil
		},
	}

	conversations := &fakeConversationStore{
		found: &model.Conversation{ID: 1, UserID: 7},
	}

	chat := &streamChatStub{
		replyStream: func(
			modelCtx context.Context,
			history []llm.HistoryMessage,
			question string,
			onChunk func(string) error,
		) (string, error) {
			if err := onChunk("完整回答"); err != nil {
				return "", err
			}

			// 模拟模型已成功完成，父请求在交接保存阶段时取消。
			cancel()
			return "完整回答", nil
		},
	}

	svc := NewMessageService(
		messages, conversations, nil, nil, chat,
	)

	result, err := svc.CreateStream(
		ctx,
		7,
		1,
		"我的问题",
		StreamCallbacks{
			OnStart: func() error { return nil },
			OnChunk: func(string) error { return nil },
		},
	)
	if err != nil {
		t.Fatalf("save after generation should succeed: %v", err)
	}

	if result == nil || result.AssistantMessage == nil {
		t.Fatal("expected saved assistant message")
	}

	if messages.saveCalls != 1 ||
		result.AssistantMessage.Content != "完整回答" {
		t.Fatal("complete answer must be saved exactly once")
	}

	if observedSaveCtx == nil {
		t.Fatal("save operation was not called")
	}

	if !errors.Is(observedSaveCtx.Err(), context.Canceled) {
		t.Fatalf(
			"save context must be canceled after service returns, got %v",
			observedSaveCtx.Err(),
		)
	}
}

func TestMessageServiceCreateStreamSaveFailure(t *testing.T) {
	dbErr := errors.New("database unavailable")

	messages := &fakeMessageStore{
		saveErr: dbErr,
	}
	conversations := &fakeConversationStore{
		found: &model.Conversation{ID: 1, UserID: 7},
	}
	contexts := &messageContextStub{}

	chat := &streamChatStub{
		replyStream: func(
			ctx context.Context,
			history []llm.HistoryMessage,
			question string,
			onChunk func(string) error,
		) (string, error) {
			if err := onChunk("完整回答"); err != nil {
				return "", err
			}
			return "完整回答", nil
		},
	}

	svc := NewMessageService(
		messages, conversations, contexts, nil, chat,
	)

	result, err := svc.CreateStream(
		context.Background(),
		7,
		1,
		"我的问题",
		StreamCallbacks{
			OnStart: func() error { return nil },
			OnChunk: func(string) error { return nil },
		},
	)

	if !errors.Is(err, dbErr) {
		t.Fatalf("expected database error, got %v", err)
	}

	if result != nil {
		t.Fatal("failed save must not return a successful result")
	}

	if messages.saveCalls != 1 {
		t.Fatalf("expected one save attempt, got %d", messages.saveCalls)
	}

	if contexts.called {
		t.Fatal("failed save must not refresh the cache")
	}

	// 保存失败也必须释放会话占用。
	release, acquired := svc.guard.TryAcquire(1)
	if !acquired {
		t.Fatal("conversation guard must be released after save failure")
	}
	release()
}

func TestMessageServiceCreateStreamCallbackFailure(t *testing.T) {
	tests := []struct {
		name           string
		failAtStart    bool
		wantModelCalls int
		wantChunkCalls int
	}{
		{
			name:           "start callback fails",
			failAtStart:    true,
			wantModelCalls: 0,
			wantChunkCalls: 0,
		},
		{
			name:           "chunk callback fails",
			wantModelCalls: 1,
			wantChunkCalls: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			writeErr := errors.New("client write failed")

			messages := &fakeMessageStore{}
			conversations := &fakeConversationStore{
				found: &model.Conversation{ID: 1, UserID: 7},
			}
			contexts := &messageContextStub{}

			modelCalls := 0
			startCalls := 0
			chunkCalls := 0

			chat := &streamChatStub{
				replyStream: func(
					ctx context.Context,
					history []llm.HistoryMessage,
					question string,
					onChunk func(string) error,
				) (string, error) {
					modelCalls++

					for _, part := range []string{"第一段", "第二段"} {
						if err := onChunk(part); err != nil {
							return "", err
						}
					}

					return "第一段第二段", nil
				},
			}

			svc := NewMessageService(
				messages, conversations, contexts, nil, chat,
			)

			result, err := svc.CreateStream(
				context.Background(),
				7,
				1,
				"我的问题",
				StreamCallbacks{
					OnStart: func() error {
						startCalls++
						if tt.failAtStart {
							return writeErr
						}
						return nil
					},
					OnChunk: func(string) error {
						chunkCalls++
						return writeErr
					},
				},
			)

			if !errors.Is(err, writeErr) {
				t.Fatalf("expected callback error, got %v", err)
			}
			if result != nil {
				t.Fatal("callback failure must not return saved messages")
			}

			if startCalls != 1 {
				t.Fatalf("expected one start callback, got %d", startCalls)
			}
			if modelCalls != tt.wantModelCalls {
				t.Fatalf(
					"expected %d model calls, got %d",
					tt.wantModelCalls, modelCalls,
				)
			}
			if chunkCalls != tt.wantChunkCalls {
				t.Fatalf(
					"expected %d chunk callbacks, got %d",
					tt.wantChunkCalls, chunkCalls,
				)
			}

			if messages.saveCalls != 0 {
				t.Fatal("callback failure must not save messages")
			}
			if contexts.called {
				t.Fatal("callback failure must not refresh the cache")
			}

			release, acquired := svc.guard.TryAcquire(1)
			if !acquired {
				t.Fatal("conversation guard must be released")
			}
			release()
		})
	}
}

func TestMessageServiceActiveStreamBlocksSameConversation(t *testing.T) {
	messages := &fakeMessageStore{}
	conversations := &fakeConversationStore{
		found: &model.Conversation{ID: 1, UserID: 7},
	}

	started := make(chan struct{})
	proceed := make(chan struct{})
	var modelCalls atomic.Int32

	chat := &streamChatStub{
		replyStream: func(
			ctx context.Context,
			history []llm.HistoryMessage,
			question string,
			onChunk func(string) error,
		) (string, error) {
			if modelCalls.Add(1) != 1 {
				return "", errors.New("unexpected concurrent model call")
			}

			close(started)

			select {
			case <-proceed:
			case <-ctx.Done():
				return "", ctx.Err()
			}

			if err := onChunk("第一条回答"); err != nil {
				return "", err
			}
			return "第一条回答", nil
		},
	}

	svc := NewMessageService(
		messages, conversations, nil, nil, chat,
	)

	ctx, cancel := context.WithTimeout(
		context.Background(),
		5*time.Second,
	)

	type requestResult struct {
		result *StreamResult
		err    error
	}

	done := make(chan requestResult, 1)
	exited := make(chan struct{})

	go func() {
		defer close(exited)

		result, err := svc.CreateStream(
			ctx,
			7,
			1,
			"第一条问题",
			StreamCallbacks{
				OnStart: func() error { return nil },
				OnChunk: func(string) error { return nil },
			},
		)

		done <- requestResult{result: result, err: err}
	}()

	defer func() {
		cancel()

		select {
		case <-exited:
		case <-time.After(2 * time.Second):
			t.Error("first request goroutine did not exit")
		}
	}()

	// 确认第一条请求已经取得会话占用，并进入模型。
	select {
	case <-started:
	case first := <-done:
		t.Fatalf("first request ended before model started: %v", first.err)
	case <-ctx.Done():
		t.Fatalf("waiting for model start: %v", ctx.Err())
	}

	// 同一 Service 实例上的普通请求也必须被拒绝。
	normalResult, err := svc.Create(
		ctx, 7, 1, "第二条普通问题",
	)
	if !errors.Is(err, ErrConversationBusy) {
		t.Fatalf("normal request: expected busy, got %v", err)
	}
	if normalResult != nil {
		t.Fatal("busy normal request must not return a message")
	}

	// 同一会话的另一条流式请求也必须被拒绝。
	secondStartCalls := 0
	secondChunkCalls := 0

	streamResult, err := svc.CreateStream(
		ctx,
		7,
		1,
		"第三条流式问题",
		StreamCallbacks{
			OnStart: func() error {
				secondStartCalls++
				return nil
			},
			OnChunk: func(string) error {
				secondChunkCalls++
				return nil
			},
		},
	)

	if !errors.Is(err, ErrConversationBusy) {
		t.Fatalf("stream request: expected busy, got %v", err)
	}
	if streamResult != nil {
		t.Fatal("busy stream request must not return a result")
	}
	if secondStartCalls != 0 || secondChunkCalls != 0 {
		t.Fatal("busy stream request must not invoke callbacks")
	}
	if modelCalls.Load() != 1 {
		t.Fatalf("expected one stream model call, got %d", modelCalls.Load())
	}

	// 两条冲突请求检查完毕，允许第一条请求正常完成。
	close(proceed)

	var first requestResult
	select {
	case first = <-done:
	case <-ctx.Done():
		t.Fatalf("waiting for first request to finish: %v", ctx.Err())
	}

	if first.err != nil {
		t.Fatalf("first request failed: %v", first.err)
	}
	if first.result == nil || first.result.AssistantMessage == nil {
		t.Fatal("first request must return saved messages")
	}

	// 第一条请求结束后，再读取非线程安全的数据库替身字段。
	if messages.saveCalls != 1 {
		t.Fatalf("expected one save, got %d", messages.saveCalls)
	}
	if messages.created == nil || messages.reply == nil {
		t.Fatal("expected a saved message pair")
	}
	if messages.created.Content != "第一条问题" ||
		messages.reply.Content != "第一条回答" {
		t.Fatal("only the first request should be saved")
	}

	release, acquired := svc.guard.TryAcquire(1)
	if !acquired {
		t.Fatal("conversation must be released after completion")
	}
	release()
}
