package middleware

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

func SafeAccessLog(logger *zap.Logger) gin.HandlerFunc {
	if logger == nil {
		logger = zap.NewNop()
	}

	return func(c *gin.Context) {
		start := time.Now()
		c.Next()

		route := c.FullPath()
		if route == "" {
			route = "unmatched"
		}

		fields := []zap.Field{
			zap.String("method", normalizedHTTPMethod(c.Request.Method)),
			zap.String("route", route),
			zap.Int("status", c.Writer.Status()),
			zap.Duration("duration", time.Since(start)),
		}
		fields = append(fields, requestSpanFields(c)...)

		logger.Info("http request", fields...)
	}
}

func SafeRecovery(logger *zap.Logger) gin.HandlerFunc {
	if logger == nil {
		logger = zap.NewNop()
	}

	return func(c *gin.Context) {
		defer func() {
			if recover() != nil {
				c.Set(httpPanicKey, true)
				logger.Error("http request panic", requestSpanFields(c)...)

				if c.Writer.Written() {
					c.Abort()
					return
				}

				c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
					"code":    "internal_error",
					"message": "internal server error",
				})
			}
		}()

		c.Next()
	}
}

func requestSpanFields(c *gin.Context) []zap.Field {
	sc := trace.SpanContextFromContext(c.Request.Context())
	if !sc.IsValid() {
		return nil
	}

	return []zap.Field{
		zap.String("trace_id", sc.TraceID().String()),
		zap.String("span_id", sc.SpanID().String()),
	}
}
