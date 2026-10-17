package middleware

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"video-support-agent/internal/auth"
	"video-support-agent/internal/cache"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

type userLimiterStub struct {
	result      cache.RateLimitResult
	err         error
	called      bool
	gotUserID   uint64
	hasDeadline bool
}

func (s *userLimiterStub) Allow(
	ctx context.Context,
	userID uint64,
) (cache.RateLimitResult, error) {
	s.called = true
	s.gotUserID = userID
	_, s.hasDeadline = ctx.Deadline()

	return s.result, s.err
}

func TestUserRateLimitMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tokens, err := auth.NewTokenManager(
		"0123456789abcdef0123456789abcdef",
		"test-support",
		time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}

	rawToken, err := tokens.Issue(7)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name             string
		header           string
		result           cache.RateLimitResult
		limiterErr       error
		wantStatus       int
		wantCode         string
		wantRetryAfter   string
		wantLimiterCall  bool
		wantHandlerCall  bool
		wantWarningCount int
	}{
		{
			name:            "allowed",
			header:          "Bearer " + rawToken,
			result:          cache.RateLimitResult{Allowed: true},
			wantStatus:      http.StatusOK,
			wantLimiterCall: true,
			wantHandlerCall: true,
		},
		{
			name:   "over limit",
			header: "Bearer " + rawToken,
			result: cache.RateLimitResult{
				Allowed:    false,
				RetryAfter: 1500 * time.Millisecond,
			},
			wantStatus:      http.StatusTooManyRequests,
			wantCode:        "rate_limited",
			wantRetryAfter:  "2",
			wantLimiterCall: true,
		},
		{
			name:            "zero remaining time",
			header:          "Bearer " + rawToken,
			result:          cache.RateLimitResult{Allowed: false},
			wantStatus:      http.StatusTooManyRequests,
			wantCode:        "rate_limited",
			wantRetryAfter:  "1",
			wantLimiterCall: true,
		},
		{
			name:             "redis failure",
			header:           "Bearer " + rawToken,
			limiterErr:       errors.New("redis unavailable"),
			wantStatus:       http.StatusServiceUnavailable,
			wantCode:         "rate_limit_unavailable",
			wantLimiterCall:  true,
			wantWarningCount: 1,
		},
		{
			name:       "missing token",
			wantStatus: http.StatusUnauthorized,
			wantCode:   "unauthorized",
		},
		{
			name:       "invalid token",
			header:     "Bearer invalid-token",
			wantStatus: http.StatusUnauthorized,
			wantCode:   "unauthorized",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			limiter := &userLimiterStub{
				result: tt.result,
				err:    tt.limiterErr,
			}
			core, logs := observer.New(zap.WarnLevel)
			handlerCalled := false

			router := gin.New()
			router.GET(
				"/private",
				JWTAuth(tokens),
				UserRateLimit(limiter, zap.New(core)),
				func(c *gin.Context) {
					handlerCalled = true
					c.Status(http.StatusOK)
				},
			)

			// 参数中的伪造用户 ID 不应影响实际限流身份。
			request := httptest.NewRequest(
				http.MethodGet,
				"/private?user_id=999",
				nil,
			)
			if tt.header != "" {
				request.Header.Set("Authorization", tt.header)
			}

			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)

			if recorder.Code != tt.wantStatus {
				t.Fatalf(
					"expected status %d, got %d",
					tt.wantStatus,
					recorder.Code,
				)
			}

			if limiter.called != tt.wantLimiterCall {
				t.Fatalf("unexpected limiter call: %v", limiter.called)
			}
			if handlerCalled != tt.wantHandlerCall {
				t.Fatalf("unexpected handler call: %v", handlerCalled)
			}

			if limiter.called {
				if limiter.gotUserID != 7 {
					t.Fatalf(
						"expected JWT user ID 7, got %d",
						limiter.gotUserID,
					)
				}
				if !limiter.hasDeadline {
					t.Fatal("limit check must have a deadline")
				}
			}

			if got := recorder.Header().Get("Retry-After"); got != tt.wantRetryAfter {
				t.Fatalf(
					"expected Retry-After %q, got %q",
					tt.wantRetryAfter,
					got,
				)
			}

			if got := logs.FilterMessage(
				"user rate limit check failed",
			).Len(); got != tt.wantWarningCount {
				t.Fatalf(
					"expected %d warnings, got %d",
					tt.wantWarningCount,
					got,
				)
			}

			if tt.wantCode != "" {
				var response struct {
					Code string `json:"code"`
				}
				if err := json.Unmarshal(
					recorder.Body.Bytes(),
					&response,
				); err != nil {
					t.Fatal(err)
				}
				if response.Code != tt.wantCode {
					t.Fatalf(
						"expected code %q, got %q",
						tt.wantCode,
						response.Code,
					)
				}
			}
		})
	}
}

func TestUserRateLimitRequiresAuthenticatedIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)

	limiter := &userLimiterStub{
		result: cache.RateLimitResult{Allowed: true},
	}
	handlerCalled := false

	router := gin.New()
	// 故意不注册 JWTAuth，验证缺少认证身份时不会放行。
	router.GET(
		"/private",
		UserRateLimit(limiter, nil),
		func(c *gin.Context) {
			handlerCalled = true
			c.Status(http.StatusOK)
		},
	)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/private", nil)
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", recorder.Code)
	}
	if limiter.called || handlerCalled {
		t.Fatal("missing identity must stop before limiter and handler")
	}
}
