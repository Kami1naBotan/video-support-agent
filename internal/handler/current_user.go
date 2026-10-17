package handler

import (
	"net/http"
	"video-support-agent/internal/middleware"
	"video-support-agent/internal/service"

	"github.com/gin-gonic/gin"
)

type CurrentUserHandler struct {
	service *service.CurrentUserService
}

func NewCurrentUserHandler(currentUserService *service.CurrentUserService) *CurrentUserHandler {
	return &CurrentUserHandler{service: currentUserService}
}

func (h *CurrentUserHandler) Me(c *gin.Context) {
	c.Header("Cache-Control", "no-store")

	userID, ok := middleware.CurrentUserID(c)
	if !ok {
		writeError(c, service.ErrUnauthorized)
		return
	}

	user, err := h.service.Get(c.Request.Context(), userID)
	if err != nil {
		writeError(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"id":       userID,
		"username": user.Username,
		"role":     user.Role,
	})
}
