package middleware

import (
	"net/http"

	"video-support-agent/internal/knowledgeevent"

	"github.com/gin-gonic/gin"
)

func RequestTrace() gin.HandlerFunc {
	return func(c *gin.Context) {
		trace, err := knowledgeevent.NewTraceID()
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
				"code":    "internal_error",
				"message": "internal server error",
			})
			return
		}

		ctx, err := knowledgeevent.WithTrace(c.Request.Context(), trace)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
				"code":    "internal_error",
				"message": "internal server error",
			})
			return
		}

		c.Request = c.Request.WithContext(ctx)
		c.Header("X-Trace-ID", trace)
		c.Next()
	}
}
