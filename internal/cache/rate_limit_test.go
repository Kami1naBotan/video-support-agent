package cache

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newTestRateLimiter(
	t *testing.T,
	limit int64,
	window time.Duration,
) (*UserRateLimiter, *miniredis.Miniredis) {
	t.Helper()

	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{
		Addr: server.Addr(),
	})
	t.Cleanup(func() {
		_ = client.Close()
	})

	limiter, err := NewUserRateLimiter(client, limit, window)
	if err != nil {
		t.Fatal(err)
	}

	return limiter, server
}

func TestUserRateLimiterLimitAndExpiry(t *testing.T) {
	limiter, server := newTestRateLimiter(t, 2, time.Minute)
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		result, err := limiter.Allow(ctx, 7)
		if err != nil {
			t.Fatal(err)
		}
		if !result.Allowed {
			t.Fatal("request within limit should be allowed")
		}
	}

	server.FastForward(40 * time.Second)

	result, err := limiter.Allow(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	if result.Allowed {
		t.Fatal("request over limit should be rejected")
	}
	if result.RetryAfter <= 0 || result.RetryAfter > 20*time.Second {
		t.Fatalf("unexpected retry delay: %v", result.RetryAfter)
	}

	// 被拒绝的请求不应该重置一分钟窗口。
	server.FastForward(21 * time.Second)

	result, err = limiter.Allow(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Allowed {
		t.Fatal("request should be allowed after window expires")
	}
}

func TestUserRateLimiterSeparatesUsers(t *testing.T) {
	limiter, _ := newTestRateLimiter(t, 1, time.Minute)
	ctx := context.Background()

	first, err := limiter.Allow(ctx, 7)
	if err != nil || !first.Allowed {
		t.Fatalf("first request failed: %v", err)
	}

	second, err := limiter.Allow(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	if second.Allowed {
		t.Fatal("user 7 should reach the limit")
	}

	other, err := limiter.Allow(ctx, 8)
	if err != nil {
		t.Fatal(err)
	}
	if !other.Allowed {
		t.Fatal("user 7 must not consume user 8's quota")
	}
}

func TestUserRateLimiterConcurrentRequests(t *testing.T) {
	limiter, _ := newTestRateLimiter(t, 5, time.Minute)

	var allowed atomic.Int64
	var failures atomic.Int64
	var wg sync.WaitGroup

	for i := 0; i < 20; i++ {
		wg.Add(1)

		go func() {
			defer wg.Done()

			result, err := limiter.Allow(context.Background(), 7)
			if err != nil {
				failures.Add(1)
				return
			}
			if result.Allowed {
				allowed.Add(1)
			}
		}()
	}

	wg.Wait()

	if failures.Load() != 0 {
		t.Fatalf("unexpected Redis errors: %d", failures.Load())
	}
	if allowed.Load() != 5 {
		t.Fatalf("expected exactly 5 allowed requests, got %d", allowed.Load())
	}
}

func TestUserRateLimiterReturnsRedisError(t *testing.T) {
	limiter, server := newTestRateLimiter(t, 2, time.Minute)

	// 计数器必须是整数，非整数内容会使 INCR 失败。
	if err := server.Set("ratelimit:user:7", "invalid-counter"); err != nil {
		t.Fatal(err)
	}

	result, err := limiter.Allow(context.Background(), 7)
	if err == nil {
		t.Fatal("expected Redis error")
	}
	if result.Allowed {
		t.Fatal("Redis error must not be reported as an allowed request")
	}
}

func TestUserRateLimiterRejectsZeroUserID(t *testing.T) {
	limiter, _ := newTestRateLimiter(t, 2, time.Minute)

	if _, err := limiter.Allow(context.Background(), 0); err == nil {
		t.Fatal("expected invalid user ID error")
	}
}

func TestNewUserRateLimiterRejectsInvalidConfig(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() {
		_ = client.Close()
	})

	if _, err := NewUserRateLimiter(nil, 1, time.Minute); err == nil {
		t.Fatal("expected nil client error")
	}
	if _, err := NewUserRateLimiter(client, 0, time.Minute); err == nil {
		t.Fatal("expected invalid limit error")
	}
	if _, err := NewUserRateLimiter(client, 1, 0); err == nil {
		t.Fatal("expected invalid window error")
	}
}
