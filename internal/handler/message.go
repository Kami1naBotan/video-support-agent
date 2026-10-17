package handler

import (
	"net/http"
	"strconv"
	"video-support-agent/internal/middleware"
	"video-support-agent/internal/model"
	"video-support-agent/internal/service"

	"github.com/gin-gonic/gin"
)

type MessageHandler struct {
	service *service.MessageService
}

func NewMessageHandler(messageService *service.MessageService) *MessageHandler {
	return &MessageHandler{
		service: messageService,
	}
}

type createMessageRequest struct {
	Content string `json:"content"`
}

func (h *MessageHandler) Create(c *gin.Context) {
	userID, ok := middleware.CurrentUserID(c)
	if !ok {
		writeError(c, service.ErrUnauthorized)
		return
	}

	conversationID, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || conversationID == 0 {
		writeError(c, service.ErrInvalidArgument)
		return
	}

	var request createMessageRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		writeBindError(c, err)
		return
	}

	message, err := h.service.Create(
		c.Request.Context(),
		userID,
		conversationID,
		request.Content,
	)
	if err != nil {
		writeError(c, err)
		return
	}

	c.JSON(http.StatusCreated, message)
}

func (h *MessageHandler) List(c *gin.Context) {
	userID, ok := middleware.CurrentUserID(c)
	if !ok {
		writeError(c, service.ErrUnauthorized)
		return
	}

	conversationID, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || conversationID == 0 {
		writeError(c, service.ErrInvalidArgument)
		return
	}

	messages, err := h.service.ListByConversationID(
		c.Request.Context(),
		userID,
		conversationID,
	)
	if err != nil {
		writeError(c, err)
		return
	}

	if messages == nil {
		messages = []model.Message{}
	}

	c.JSON(http.StatusOK, messages)
}
