package cache

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestAuthIPRateLimit(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() {
		_ = client.Close()
	})

	limiter, err := NewUserRateLimiter(client, 1, time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()

	first, err := limiter.AllowIP(ctx, "192.0.2.1")
	if err != nil || !first.Allowed {
		t.Fatalf("first request: result=%+v err=%v", first, err)
	}

	second, err := limiter.AllowIP(ctx, "192.0.2.1")
	if err != nil || second.Allowed || second.RetryAfter <= 0 {
		t.Fatalf("second request: result=%+v err=%v", second, err)
	}

	other, err := limiter.AllowIP(ctx, "192.0.2.2")
	if err != nil || !other.Allowed {
		t.Fatalf("other IP: result=%+v err=%v", other, err)
	}

	user, err := limiter.Allow(ctx, 1)
	if err != nil || !user.Allowed {
		t.Fatalf("user namespace: result=%+v err=%v", user, err)
	}

	server.FastForward(time.Minute + time.Second)

	reset, err := limiter.AllowIP(ctx, "192.0.2.1")
	if err != nil || !reset.Allowed {
		t.Fatalf("after expiry: result=%+v err=%v", reset, err)
	}

	if _, err := limiter.AllowIP(ctx, "invalid"); err == nil {
		t.Fatal("expected invalid IP error")
	}
}
