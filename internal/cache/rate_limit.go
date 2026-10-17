package cache

import (
	"context"
	"fmt"
	"net/netip"
	"time"

	"github.com/redis/go-redis/v9"
)

type RateLimitResult struct {
	Allowed    bool
	RetryAfter time.Duration
}

type UserRateLimiter struct {
	client *redis.Client
	limit  int64
	window time.Duration
}

func NewUserRateLimiter(client *redis.Client, limit int64, window time.Duration) (*UserRateLimiter, error) {
	if client == nil {
		return nil, fmt.Errorf("redis client is required")
	}
	if limit <= 0 {
		return nil, fmt.Errorf("rate limit must be positive")
	}
	if window < time.Second {
		return nil, fmt.Errorf("rate limit window must be at least one second")
	}

	return &UserRateLimiter{
		client: client,
		limit:  limit,
		window: window,
	}, nil
}

var userRateLimitScript = redis.NewScript(`
local count = redis.call("INCR", KEYS[1])
local ttl = redis.call("PTTL", KEYS[1])

if count == 1 or ttl < 0 then
	redis.call("PEXPIRE", KEYS[1], ARGV[1])
	ttl = tonumber(ARGV[1])
end

return {count, ttl}
`)

func (l *UserRateLimiter) allowKey(ctx context.Context, key string) (RateLimitResult, error) {
	values, err := userRateLimitScript.Run(ctx, l.client, []string{key}, l.window.Milliseconds()).Slice()
	if err != nil {
		return RateLimitResult{}, fmt.Errorf("check rate limit: %w", err)
	}

	if len(values) != 2 {
		return RateLimitResult{}, fmt.Errorf("unexpected rate limit result")
	}

	count, countOK := values[0].(int64)
	ttlMillis, ttlOK := values[1].(int64)
	if !countOK || !ttlOK || count < 1 || ttlMillis < 0 {
		return RateLimitResult{}, fmt.Errorf("invalid rate limit result")
	}

	result := RateLimitResult{
		Allowed: count <= l.limit,
	}
	if !result.Allowed {
		result.RetryAfter = time.Duration(ttlMillis) * time.Millisecond
	}

	return result, nil
}

func (l *UserRateLimiter) Allow(ctx context.Context, userID uint64) (RateLimitResult, error) {
	if userID == 0 {
		return RateLimitResult{}, fmt.Errorf("user ID must be positive")
	}

	key := fmt.Sprintf("ratelimit:user:%d", userID)
	return l.allowKey(ctx, key)
}

func (l *UserRateLimiter) AllowIP(ctx context.Context, ip string) (RateLimitResult, error) {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return RateLimitResult{}, fmt.Errorf("invalid client IP")
	}

	key := "ratelimit:auth:ip:" + addr.Unmap().String()
	return l.allowKey(ctx, key)
}
