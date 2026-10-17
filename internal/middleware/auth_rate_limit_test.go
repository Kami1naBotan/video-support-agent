package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"video-support-agent/internal/cache"

	"github.com/gin-gonic/gin"
)

type authIPLimiterStub struct {
	result      cache.RateLimitResult
	err         error
	ip          string
	hasDeadline bool
}

func (s *authIPLimiterStub) AllowIP(
	ctx context.Context,
	ip string,
) (cache.RateLimitResult, error) {
	s.ip = ip
	_, s.hasDeadline = ctx.Deadline()
	return s.result, s.err
}

func TestAuthRateLimit(t *testing.T) {
	tests := []struct {
		name       string
		result     cache.RateLimitResult
		err        error
		wantStatus int
		wantRetry  string
	}{
		{
			name:       "allowed",
			result:     cache.RateLimitResult{Allowed: true},
			wantStatus: http.StatusNoContent,
		},
		{
			name: "limited",
			result: cache.RateLimitResult{
				RetryAfter: 1500 * time.Millisecond,
			},
			wantStatus: http.StatusTooManyRequests,
			wantRetry:  "2",
		},
		{
			name:       "redis unavailable",
			err:        errors.New("redis unavailable"),
			wantStatus: http.StatusServiceUnavailable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stub := &authIPLimiterStub{
				result: tt.result,
				err:    tt.err,
			}

			r := gin.New()
			if err := r.SetTrustedProxies(nil); err != nil {
				t.Fatal(err)
			}
			r.Use(AuthRateLimit(stub, nil))

			called := false
			r.POST("/", func(c *gin.Context) {
				called = true
				c.Status(http.StatusNoContent)
			})

			req := httptest.NewRequest(http.MethodPost, "/", nil)
			req.RemoteAddr = "192.0.2.10:12345"
			req.Header.Set("X-Forwarded-For", "198.51.100.99")

			recorder := httptest.NewRecorder()
			r.ServeHTTP(recorder, req)

			if recorder.Code != tt.wantStatus {
				t.Fatalf("expected %d, got %d", tt.wantStatus, recorder.Code)
			}
			if called != (tt.wantStatus == http.StatusNoContent) {
				t.Fatalf("unexpected handler execution: %v", called)
			}
			if stub.ip != "192.0.2.10" {
				t.Fatalf("unexpected client IP: %q", stub.ip)
			}
			if !stub.hasDeadline {
				t.Fatal("expected limiter context deadline")
			}
			if recorder.Header().Get("Retry-After") != tt.wantRetry {
				t.Fatalf("unexpected Retry-After header")
			}
		})
	}
}
