package middleware

import (
	"net/http"
	"strings"
	"video-support-agent/internal/auth"

	"github.com/gin-gonic/gin"
)

const userIDKey = "auth.user_id"

func JWTAuth(tokens *auth.TokenManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		parts := strings.Fields(c.GetHeader("Authorization"))

		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			abortUnauthorized(c)
			return
		}

		userID, err := tokens.Parse(parts[1])
		if err != nil {
			abortUnauthorized(c)
			return
		}

		c.Set(userIDKey, userID)
		c.Next()
	}
}

func CurrentUserID(c *gin.Context) (uint64, bool) {
	value, exists := c.Get(userIDKey)
	if !exists {
		return 0, false
	}

	userID, ok := value.(uint64)
	return userID, ok && userID > 0
}

func abortUnauthorized(c *gin.Context) {
	c.Header("WWW-Authenticate", "Bearer")
	c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
		"code":    "unauthorized",
		"message": "authentication required or token invalid",
	})
}
