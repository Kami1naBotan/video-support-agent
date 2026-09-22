package handler

import (
	"net/http"
	"strconv"
	"video-support-agent/internal/service"

	"github.com/gin-gonic/gin"
)

type ConversationHandler struct {
	service *service.ConversationService
}

func NewConversationHandler(conversationService *service.ConversationService) *ConversationHandler {
	return &ConversationHandler{
		service: conversationService,
	}
}

type createConversationRequest struct {
	UserID uint64 `json:"user_id"`
	Title  string `json:"title"`
}

func (h *ConversationHandler) Create(c *gin.Context) {
	var request createConversationRequest

	if err := c.ShouldBindJSON(&request); err != nil {
		writeError(c, service.ErrInvalidArgument)
		return
	}

	conversation, err := h.service.Create(
		c.Request.Context(),
		request.UserID,
		request.Title,
	)
	if err != nil {
		writeError(c, err)
		return
	}

	c.JSON(http.StatusCreated, conversation)
}

func (h *ConversationHandler) GetByID(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || id == 0 {
		writeError(c, service.ErrInvalidArgument)
		return
	}

	conversation, err := h.service.GetByID(c.Request.Context(), id)
	if err != nil {
		writeError(c, err)
		return
	}

	c.JSON(http.StatusOK, conversation)
}
