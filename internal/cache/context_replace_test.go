package cache

import (
	"context"
	"testing"
	"time"
)

func TestContextStoreReplaceKeepsLatest(t *testing.T) {
	store, _ := newTestContextStore(t, 2, time.Minute)
	ctx := context.Background()

	if err := store.Append(ctx, 10, ContextMessage{
		ID: 1, Role: "user", Content: "旧缓存",
	}); err != nil {
		t.Fatal(err)
	}

	err := store.Replace(ctx, 10, []ContextMessage{
		{ID: 2, Role: "assistant", Content: "较早的回答"},
		{ID: 3, Role: "user", Content: "新的问题"},
		{ID: 4, Role: "assistant", Content: "新的回答"},
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := store.List(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(got))
	}
	if got[0].ID != 3 || got[1].ID != 4 {
		t.Fatalf("unexpected message order: %+v", got)
	}
	if got[0].Role != "user" || got[0].Content != "新的问题" ||
		got[1].Role != "assistant" || got[1].Content != "新的回答" {
		t.Fatalf("message fields were not preserved: %+v", got)
	}
}

func TestContextStoreReplaceRefreshesTTL(t *testing.T) {
	store, server := newTestContextStore(t, 20, time.Minute)
	ctx := context.Background()

	if err := store.Append(ctx, 10, ContextMessage{
		ID: 1, Role: "user", Content: "旧消息",
	}); err != nil {
		t.Fatal(err)
	}

	server.FastForward(40 * time.Second)

	if err := store.Replace(ctx, 10, []ContextMessage{
		{ID: 2, Role: "assistant", Content: "替换后的消息"},
	}); err != nil {
		t.Fatal(err)
	}

	server.FastForward(30 * time.Second)

	got, err := store.List(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != 2 {
		t.Fatal("replacement must refresh the TTL")
	}

	server.FastForward(31 * time.Second)

	got, err = store.List(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatal("reading must not refresh the TTL")
	}
}

func TestContextStoreReplaceEmptyClearsOnlyTarget(t *testing.T) {
	store, server := newTestContextStore(t, 20, time.Minute)
	ctx := context.Background()

	for _, conversationID := range []uint64{10, 20} {
		if err := store.Append(ctx, conversationID, ContextMessage{
			ID: conversationID, Role: "user", Content: "已有历史",
		}); err != nil {
			t.Fatal(err)
		}
	}

	if err := store.Replace(ctx, 10, nil); err != nil {
		t.Fatal(err)
	}

	if server.Exists(contextKey(10)) {
		t.Fatal("empty replacement must remove the target key")
	}

	got, err := store.List(ctx, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != 20 {
		t.Fatal("replacement must not affect another conversation")
	}
}

func TestContextStoreReplaceInvalidInputPreservesCache(t *testing.T) {
	store, _ := newTestContextStore(t, 20, time.Minute)
	ctx := context.Background()

	old := ContextMessage{
		ID: 1, Role: "user", Content: "需要保留的旧历史",
	}
	if err := store.Append(ctx, 10, old); err != nil {
		t.Fatal(err)
	}

	if err := store.Replace(ctx, 0, []ContextMessage{old}); err == nil {
		t.Fatal("expected invalid conversation ID error")
	}

	err := store.Replace(ctx, 10, []ContextMessage{
		{ID: 2, Role: "user", Content: "合法消息"},
		{ID: 0, Role: "assistant", Content: "没有数据库 ID 的消息"},
	})
	if err == nil {
		t.Fatal("expected invalid message ID error")
	}

	got, err := store.List(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != old {
		t.Fatalf("invalid replacement damaged existing history: %+v", got)
	}
}
