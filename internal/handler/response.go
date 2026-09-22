package handler

import (
	"errors"
	"net/http"
	"video-support-agent/internal/service"

	"github.com/gin-gonic/gin"
)

type ErrorResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeError(c *gin.Context, err error) {
	status := http.StatusInternalServerError
	code := "internal_error"
	message := "internal server error"

	switch {
	case errors.Is(err, service.ErrInvalidArgument):
		status = http.StatusBadRequest
		code = "invalid_argument"
		message = "invalid request"
	case errors.Is(err, service.ErrUserNotFound):
		status = http.StatusNotFound
		code = "user_not_found"
		message = "user not found"
	case errors.Is(err, service.ErrConversationNotFound):
		status = http.StatusNotFound
		code = "conversation_not_found"
		message = "conversation not found"
	case errors.Is(err, service.ErrMessageNotFound):
		status = http.StatusNotFound
		code = "message_not_found"
		message = "message not found"
	}

	c.JSON(status, ErrorResponse{
		Code:    code,
		Message: message,
	})
}
