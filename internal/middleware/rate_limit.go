package middleware

import (
	"context"
	"net/http"
	"strconv"
	"time"
	"video-support-agent/internal/cache"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

type UserLimiter interface {
	Allow(context.Context, uint64) (cache.RateLimitResult, error)
}

func UserRateLimit(limiter UserLimiter, logger *zap.Logger) gin.HandlerFunc {
	if logger == nil {
		logger = zap.NewNop()
	}

	return func(c *gin.Context) {
		userID, ok := CurrentUserID(c)
		if !ok {
			abortUnauthorized(c)
			return
		}

		ctx, cancel := context.WithTimeout(c.Request.Context(), 500*time.Millisecond)
		result, err := limiter.Allow(ctx, userID)
		cancel()

		if err != nil {
			logger.Warn(
				"user rate limit check failed",
				zap.Uint64("user_id", userID),
				zap.Error(err),
			)

			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
				"code":    "rate_limit_unavailable",
				"message": "service temporarily unavailable",
			})
			return
		}

		if !result.Allowed {
			seconds := int64(result.RetryAfter / time.Second)
			if result.RetryAfter%time.Second > 0 {
				seconds++
			}
			if seconds < 1 {
				seconds = 1
			}

			c.Header("Retry-After", strconv.FormatInt(int64(seconds), 10))
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"code":    "rate_limited",
				"message": "too many requests",
			})
			return
		}

		c.Next()
	}
}
