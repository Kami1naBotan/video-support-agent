package handler

import (
	"net/http"
	"strconv"
	"video-support-agent/internal/middleware"
	"video-support-agent/internal/model"
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
	Title string `json:"title"`
}

func (h *ConversationHandler) Create(c *gin.Context) {
	userID, ok := middleware.CurrentUserID(c)
	if !ok {
		writeError(c, service.ErrUnauthorized)
		return
	}

	var request createConversationRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		writeBindError(c, err)
		return
	}

	conversation, err := h.service.Create(
		c.Request.Context(),
		userID,
		request.Title,
	)
	if err != nil {
		writeError(c, err)
		return
	}

	c.JSON(http.StatusCreated, conversation)
}

func (h *ConversationHandler) GetByID(c *gin.Context) {
	userID, ok := middleware.CurrentUserID(c)
	if !ok {
		writeError(c, service.ErrUnauthorized)
		return
	}

	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || id == 0 {
		writeError(c, service.ErrInvalidArgument)
		return
	}

	conversation, err := h.service.GetByID(c.Request.Context(), userID, id)
	if err != nil {
		writeError(c, err)
		return
	}

	c.JSON(http.StatusOK, conversation)
}

func (h *ConversationHandler) List(c *gin.Context) {
	userID, ok := middleware.CurrentUserID(c)
	if !ok {
		writeError(c, service.ErrUnauthorized)
		return
	}

	conversations, err := h.service.ListByUserID(c.Request.Context(), userID)
	if err != nil {
		writeError(c, err)
		return
	}

	if conversations == nil {
		conversations = []model.Conversation{}
	}

	c.JSON(http.StatusOK, conversations)
}
