package handler

import (
	"context"
	"errors"
	"net/http"
	"video-support-agent/internal/llm"
	"video-support-agent/internal/service"

	"github.com/gin-gonic/gin"
)

type ErrorResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func classifyResponseError(err error) (int, ErrorResponse) {
	status := http.StatusInternalServerError
	code := "internal_error"
	message := "internal server error"
	switch {
	case errors.Is(err, llm.ErrModelTimeout):
		status = http.StatusGatewayTimeout
		code = "model_timeout"
		message = "模型响应超时，请稍后重试"
	case errors.Is(err, llm.ErrModelFailed):
		status = http.StatusBadGateway
		code = "model_unavailable"
		message = "模型服务暂时不可用，请稍后重试"
	case errors.Is(err, llm.ErrEmptyReply):
		status = http.StatusBadGateway
		code = "model_empty_reply"
		message = "模型未返回有效回答，请稍后重试"
	case errors.Is(err, context.DeadlineExceeded):
		status = http.StatusGatewayTimeout
		code = "request_timeout"
		message = "request timed out"
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
	case errors.Is(err, service.ErrUsernameExists):
		status = http.StatusConflict
		code = "username_exists"
		message = "username already exists"
	case errors.Is(err, service.ErrInvalidCredentials):
		status = http.StatusUnauthorized
		code = "invalid_credentials"
		message = "invalid username or password"
	case errors.Is(err, service.ErrUnauthorized):
		status = http.StatusUnauthorized
		code = "unauthorized"
		message = "authentication required"
	case errors.Is(err, service.ErrConversationBusy):
		status = http.StatusConflict
		code = "conversation_busy"
		message = "该会话正在处理上一条消息，请等待完成后再发送"
	case errors.Is(err, llm.ErrInputTooLarge):
		status = http.StatusBadRequest
		code = "model_input_too_large"
		message = "问题内容超过模型输入预算，请缩短后重试"
	case errors.Is(err, llm.ErrStreamUnsupported):
		status = http.StatusServiceUnavailable
		code = "stream_unavailable"
		message = "流式服务暂时不可用"
	case errors.Is(err, llm.ErrIncompleteReply):
		status = http.StatusBadGateway
		code = "model_incomplete_reply"
		message = "模型回答未完整生成，本轮未保存，请稍后重试"
	}

	return status, ErrorResponse{
		Code:    code,
		Message: message,
	}
}

func writeError(c *gin.Context, err error) {
	if errors.Is(err, context.Canceled) {
		return
	}

	status, response := classifyResponseError(err)

	if errors.Is(err, service.ErrUnauthorized) {
		c.Header("WWW-Authenticate", "Bearer")
	}

	c.JSON(status, response)
}

func writeBindError(c *gin.Context, err error) {

	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		c.JSON(http.StatusRequestEntityTooLarge, ErrorResponse{
			Code:    "request_too_large",
			Message: "request body too large",
		})
		return
	}

	writeError(c, service.ErrInvalidArgument)
}
