package cache

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newTestContextStore(
	t *testing.T,
	maxMessages int64,
	ttl time.Duration,
) (*ContextStore, *miniredis.Miniredis) {
	t.Helper()

	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{
		Addr: server.Addr(),
	})
	t.Cleanup(func() {
		_ = client.Close()
	})

	store, err := NewContextStore(client, maxMessages, ttl)
	if err != nil {
		t.Fatal(err)
	}

	return store, server
}

func TestContextStoreKeepsLatestMessages(t *testing.T) {
	store, _ := newTestContextStore(t, 2, 30*time.Minute)
	ctx := context.Background()

	for _, message := range []ContextMessage{
		{ID: 1, Role: "user", Content: "第一条"},
		{ID: 2, Role: "user", Content: "第二条"},
		{ID: 3, Role: "user", Content: "第三条🙂"},
	} {
		if err := store.Append(ctx, 10, message); err != nil {
			t.Fatal(err)
		}
	}

	got, err := store.List(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(got))
	}
	if got[0].ID != 2 || got[1].ID != 3 {
		t.Fatalf("unexpected message order: %+v", got)
	}
	if got[1].Content != "第三条🙂" || got[1].Role != "user" {
		t.Fatal("message fields were not preserved")
	}
}

func TestContextStoreSeparatesConversations(t *testing.T) {
	store, _ := newTestContextStore(t, 20, 30*time.Minute)
	ctx := context.Background()

	if err := store.Append(ctx, 10, ContextMessage{
		ID: 1, Role: "user", Content: "conversation A",
	}); err != nil {
		t.Fatal(err)
	}

	if err := store.Append(ctx, 20, ContextMessage{
		ID: 2, Role: "user", Content: "conversation B",
	}); err != nil {
		t.Fatal(err)
	}

	for conversationID, wantID := range map[uint64]uint64{
		10: 1,
		20: 2,
	} {
		got, err := store.List(ctx, conversationID)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].ID != wantID {
			t.Fatalf("unexpected context for conversation %d", conversationID)
		}
	}
}

func TestContextStoreRefreshesTTLAndExpires(t *testing.T) {
	store, server := newTestContextStore(t, 20, time.Minute)
	ctx := context.Background()

	if err := store.Append(ctx, 10, ContextMessage{
		ID: 1, Role: "user", Content: "first",
	}); err != nil {
		t.Fatal(err)
	}

	server.FastForward(40 * time.Second)

	if err := store.Append(ctx, 10, ContextMessage{
		ID: 2, Role: "user", Content: "second",
	}); err != nil {
		t.Fatal(err)
	}

	// 距首次写入已过 70 秒，但距第二次写入只有 30 秒。
	server.FastForward(30 * time.Second)

	got, err := store.List(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatal("append should refresh the TTL")
	}

	// 读取不应刷新 TTL，第二次写入后超过 60 秒应过期。
	server.FastForward(31 * time.Second)

	got, err = store.List(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatal("expired context should be empty")
	}
}

func TestContextStoreMissingKey(t *testing.T) {
	store, _ := newTestContextStore(t, 20, time.Minute)

	got, err := store.List(context.Background(), 999)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || len(got) != 0 {
		t.Fatal("missing context should return an empty slice")
	}
}

func TestContextStoreReturnsRedisErrors(t *testing.T) {
	store, server := newTestContextStore(t, 20, time.Minute)

	// 在列表键写入字符串，模拟键类型异常。
	if err := server.Set(contextKey(10), "not-a-list"); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	if _, err := store.List(ctx, 10); err == nil {
		t.Fatal("expected read error for wrong Redis type")
	}

	if err := store.Append(ctx, 10, ContextMessage{
		ID: 1, Role: "user", Content: "test",
	}); err == nil {
		t.Fatal("expected append error for wrong Redis type")
	}
}

func TestContextStoreRejectsInvalidIDs(t *testing.T) {
	store, _ := newTestContextStore(t, 20, time.Minute)
	ctx := context.Background()

	if _, err := store.List(ctx, 0); err == nil {
		t.Fatal("expected invalid conversation ID error")
	}
	if err := store.Append(ctx, 0, ContextMessage{ID: 1}); err == nil {
		t.Fatal("expected invalid conversation ID error")
	}
	if err := store.Append(ctx, 10, ContextMessage{ID: 0}); err == nil {
		t.Fatal("expected invalid message ID error")
	}
}
