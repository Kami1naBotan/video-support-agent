//go:build redisintegration

package cache

import (
	"context"
	"net"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func liveRedis(t *testing.T) (*redis.Client, context.Context) {
	t.Helper()
	addr, owner := os.Getenv("CACHE_TEST_ADDR"), os.Getenv("CACHE_TEST_OWNER")
	require.NotEmpty(t, owner, "run the isolated Redis script")
	host, _, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	require.Equal(t, "127.0.0.1", host)
	client := redis.NewClient(&redis.Options{Addr: addr, MaxRetries: -1, DialTimeout: time.Second, ReadTimeout: time.Second, WriteTimeout: time.Second})
	t.Cleanup(func() { _ = client.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	value, err := client.Get(ctx, "test:owner").Result()
	require.NoError(t, err)
	require.Equal(t, owner, value, "unexpected Redis identity")
	return client, ctx
}

func TestRedisLiveConcurrentLimit(t *testing.T) {
	c, ctx := liveRedis(t)
	l, err := NewUserRateLimiter(c, 10, 30*time.Second)
	require.NoError(t, err)
	type outcome struct {
		result RateLimitResult
		err    error
	}
	start := make(chan struct{})
	results := make(chan outcome, 30)
	for i := 0; i < 30; i++ {
		go func() { <-start; r, e := l.Allow(ctx, 101); results <- outcome{r, e} }()
	}
	close(start)
	allowed := 0
	for i := 0; i < 30; i++ {
		select {
		case got := <-results:
			require.NoError(t, got.err)
			if got.result.Allowed {
				allowed++
			} else {
				require.Greater(t, got.result.RetryAfter, time.Duration(0))
				require.LessOrEqual(t, got.result.RetryAfter, 30*time.Second)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	require.Equal(t, 10, allowed)
	count, err := c.Get(ctx, "ratelimit:user:101").Int()
	require.NoError(t, err)
	require.Equal(t, 30, count)
	other, err := l.Allow(ctx, 102)
	require.NoError(t, err)
	require.True(t, other.Allowed)
}

func TestRedisLiveLimitExpiry(t *testing.T) {
	c, ctx := liveRedis(t)
	l, err := NewUserRateLimiter(c, 1, time.Second)
	require.NoError(t, err)
	r, err := l.Allow(ctx, 201)
	require.NoError(t, err)
	require.True(t, r.Allowed)
	r, err = l.Allow(ctx, 201)
	require.NoError(t, err)
	require.False(t, r.Allowed)
	require.Greater(t, r.RetryAfter, time.Duration(0))
	require.Eventually(t, func() bool { n, e := c.Exists(ctx, "ratelimit:user:201").Result(); return e == nil && n == 0 }, 5*time.Second, 20*time.Millisecond)
	r, err = l.Allow(ctx, 201)
	require.NoError(t, err)
	require.True(t, r.Allowed)
}

func TestRedisLiveContextSnapshot(t *testing.T) {
	c, ctx := liveRedis(t)
	s, err := NewContextStore(c, 2, 30*time.Second)
	require.NoError(t, err)
	messages := []ContextMessage{{ID: 1, Role: "user", Content: "one"}, {ID: 2, Role: "assistant", Content: "two"}, {ID: 3, Role: "user", Content: "three"}}
	for _, m := range messages {
		require.NoError(t, s.Append(ctx, 301, m))
	}
	got, err := s.List(ctx, 301)
	require.NoError(t, err)
	require.Equal(t, messages[1:], got)
	require.NoError(t, s.Replace(ctx, 302, messages[:1]))
	replacement := []ContextMessage{{ID: 4, Role: "assistant", Content: "four"}, {ID: 5, Role: "user", Content: "five"}, {ID: 6, Role: "assistant", Content: "six"}}
	require.NoError(t, s.Replace(ctx, 301, replacement))
	got, err = s.List(ctx, 301)
	require.NoError(t, err)
	require.Equal(t, replacement[1:], got)
	require.Error(t, s.Replace(ctx, 301, []ContextMessage{{ID: 7}, {ID: 0}}))
	got, err = s.List(ctx, 301)
	require.NoError(t, err)
	require.Equal(t, replacement[1:], got)
	require.NoError(t, s.Replace(ctx, 301, nil))
	n, err := c.Exists(ctx, contextKey(301)).Result()
	require.NoError(t, err)
	require.Zero(t, n)
	got, err = s.List(ctx, 302)
	require.NoError(t, err)
	require.Equal(t, messages[:1], got)
}

func TestRedisLiveContextTTL(t *testing.T) {
	c, ctx := liveRedis(t)
	s, err := NewContextStore(c, 2, 2*time.Second)
	require.NoError(t, err)
	for i, update := range []string{"append", "replace"} {
		t.Run(update, func(t *testing.T) {
			id := uint64(400 + i)
			require.NoError(t, s.Append(ctx, id, ContextMessage{ID: 1, Role: "user", Content: "old"}))
			require.Eventually(t, func() bool {
				ttl, e := c.PTTL(ctx, contextKey(id)).Result()
				return e == nil && ttl > 0 && ttl < time.Second
			}, 4*time.Second, 20*time.Millisecond)
			m := ContextMessage{ID: 2, Role: "assistant", Content: "new"}
			if update == "append" {
				require.NoError(t, s.Append(ctx, id, m))
			} else {
				require.NoError(t, s.Replace(ctx, id, []ContextMessage{m}))
			}
			ttl, err := c.PTTL(ctx, contextKey(id)).Result()
			require.NoError(t, err)
			require.Greater(t, ttl, time.Second)
			require.LessOrEqual(t, ttl, 2*time.Second)
			// Repeated reads must not renew the TTL. No virtual clock is used.
			require.Eventually(t, func() bool { rows, e := s.List(ctx, id); return e == nil && len(rows) == 0 }, 5*time.Second, 30*time.Millisecond)
		})
	}
}

func TestRedisLiveCancellationAndErrors(t *testing.T) {
	c, ctx := liveRedis(t)
	s, err := NewContextStore(c, 2, time.Minute)
	require.NoError(t, err)
	l, err := NewUserRateLimiter(c, 1, time.Minute)
	require.NoError(t, err)
	old := []ContextMessage{{ID: 1, Role: "user", Content: "preserve"}}
	require.NoError(t, s.Replace(ctx, 501, old))
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	require.ErrorIs(t, s.Replace(canceled, 501, []ContextMessage{{ID: 2}}), context.Canceled)
	require.ErrorIs(t, s.Append(canceled, 501, ContextMessage{ID: 3}), context.Canceled)
	_, err = s.List(canceled, 501)
	require.ErrorIs(t, err, context.Canceled)
	_, err = l.Allow(canceled, 501)
	require.ErrorIs(t, err, context.Canceled)
	got, err := s.List(ctx, 501)
	require.NoError(t, err)
	require.Equal(t, old, got)
	n, err := c.Exists(ctx, "ratelimit:user:501").Result()
	require.NoError(t, err)
	require.Zero(t, n)
	// A real Redis protocol error must not be disguised as an empty cache.
	require.NoError(t, c.Set(ctx, contextKey(502), "wrong-type", 0).Err())
	_, err = s.List(ctx, 502)
	require.ErrorContains(t, err, "WRONGTYPE")
}

func TestRedisLiveDisconnected(t *testing.T) {
	c, ctx := liveRedis(t)
	// Explicit destructive-test gate, after loopback and owner verification.
	require.Equal(t, "1", os.Getenv("CACHE_TEST_STOP_SERVER"))
	s, err := NewContextStore(c, 2, time.Minute)
	require.NoError(t, err)
	l, err := NewUserRateLimiter(c, 1, time.Minute)
	require.NoError(t, err)
	_ = c.Do(ctx, "SHUTDOWN", "NOSAVE").Err()
	require.Eventually(t, func() bool { return c.Ping(ctx).Err() != nil }, 5*time.Second, 50*time.Millisecond)
	require.Error(t, s.Append(ctx, 601, ContextMessage{ID: 1}))
	require.Error(t, s.Replace(ctx, 601, []ContextMessage{{ID: 1}}))
	rows, err := s.List(ctx, 601)
	require.Error(t, err)
	require.Nil(t, rows)
	r, err := l.Allow(ctx, 601)
	require.Error(t, err)
	require.False(t, r.Allowed)
}
