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
	reads   *service.AgentReadService
	agent   bool
}

func NewMessageHandler(messageService *service.MessageService) *MessageHandler {
	return &MessageHandler{
		service: messageService,
	}
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

	request, err := decodeMessageRequest(c)
	if err != nil {
		writeBindError(c, err)
		return
	}

	if h.agent {
		h.createAgent(c, userID, conversationID, request)
		return
	}

	if request.Confirmed != nil {
		writeError(c, service.ErrAgentAuditConfiguration)
		return
	}

	message, err := h.service.Create(c.Request.Context(), userID, conversationID, request.Content)
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

	if h.agent {
		messages, err := h.reads.ListHistory(c.Request.Context(), userID, conversationID)
		if err != nil {
			writeError(c, err)
			return
		}
		if messages == nil {
			messages = []service.MessageHistoryView{}
		}

		c.Header("Cache-Control", "no-store")
		c.JSON(http.StatusOK, messages)
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
