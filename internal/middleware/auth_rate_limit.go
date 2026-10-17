package middleware

import (
	"context"
	"math"
	"net/http"
	"strconv"
	"time"

	"video-support-agent/internal/cache"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

type AuthIPLimiter interface {
	AllowIP(context.Context, string) (cache.RateLimitResult, error)
}

func AuthRateLimit(
	limiter AuthIPLimiter,
	logger *zap.Logger,
) gin.HandlerFunc {
	if logger == nil {
		logger = zap.NewNop()
	}

	return func(c *gin.Context) {
		if limiter == nil {
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
				"code":    "rate_limit_unavailable",
				"message": "authentication temporarily unavailable",
			})
			return
		}

		ctx, cancel := context.WithTimeout(
			c.Request.Context(),
			500*time.Millisecond,
		)
		result, err := limiter.AllowIP(ctx, c.ClientIP())
		cancel()

		if err != nil {
			logger.Warn(
				"authentication rate limit failed",
				zap.String("operation", "auth_rate_limit"),
			)

			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
				"code":    "rate_limit_unavailable",
				"message": "authentication temporarily unavailable",
			})
			return
		}

		if !result.Allowed {
			seconds := int(math.Ceil(result.RetryAfter.Seconds()))
			if seconds < 1 {
				seconds = 1
			}

			c.Header("Retry-After", strconv.Itoa(seconds))
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"code":    "rate_limited",
				"message": "too many requests",
			})
			return
		}

		c.Next()
	}
}
