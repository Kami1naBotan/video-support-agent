package middleware

import (
	"context"
	"errors"
	"net/http"
	"video-support-agent/internal/model"
	"video-support-agent/internal/service"

	"github.com/gin-gonic/gin"
)

type CurrentUserReader interface {
	Get(context.Context, uint64) (*model.User, error)
}

func RequireRoles(users CurrentUserReader, roles ...string) gin.HandlerFunc {
	allowed := make(map[string]bool, len(roles))
	for _, role := range roles {
		switch role {
		case model.RoleUser, model.RoleSupport, model.RoleAdmin:
			allowed[role] = true
		}
	}

	return func(c *gin.Context) {
		userID, ok := CurrentUserID(c)
		if !ok {
			abortUnauthorized(c)
			return
		}

		user, err := users.Get(c.Request.Context(), userID)
		if err != nil {
			if errors.Is(err, service.ErrUnauthorized) {
				abortUnauthorized(c)
				return
			}

			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
				"code":    "internal_error",
				"message": "internal server error",
			})
			return
		}

		if !allowed[user.Role] {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"code":    "forbidden",
				"message": "insufficient permissions",
			})
			return
		}

		c.Next()
	}
}
