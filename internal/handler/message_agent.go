package handler

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"video-support-agent/internal/middleware"
	"video-support-agent/internal/model"
	"video-support-agent/internal/service"

	"github.com/gin-gonic/gin"
)

const agentResponseReadTimeout = 5 * time.Second

func NewAgentMessageHandler(messages *service.MessageService, reads *service.AgentReadService) (*MessageHandler, error) {
	if messages == nil || reads == nil {
		return nil, service.ErrAgentAuditConfiguration
	}

	return &MessageHandler{
		service: messages,
		reads:   reads,
		agent:   true,
	}, nil
}

func (h *MessageHandler) createAgent(c *gin.Context, userID uint64, conversationID uint64, request messageHTTPRequest) {
	result, executeErr := h.service.CreateAgent(
		c.Request.Context(),
		service.AgentMessageInput{
			UserID:         userID,
			ConversationID: conversationID,
			Content:        request.Content,
			Confirmed:      request.Confirmed,
		},
		service.AgentMessageCallbacks{},
	)

	body := agentMessageResponse{}
	if result != nil {
		body.Agent = projectAgentResult(result.Outcome)

		if result.Outcome != nil &&
			result.Outcome.MessageSaveStatus == "saved" &&
			result.Receipt != nil &&
			result.Receipt.UserMessageID != 0 &&
			result.Receipt.AssistantMessageID != 0 {
			receipt := *result.Receipt
			body.Messages = &receipt
			body.Message = result.UserMessage
		}
	}

	if executeErr == nil {
		if body.Agent == nil {
			executeErr = service.ErrAgentAuditUnavailable
		} else if body.Messages == nil {
			executeErr = service.ErrAgentMessageSave
		} else if body.Message == nil {
			body.Message, executeErr = h.readSavedUserMessage(
				c.Request.Context(),
				userID,
				conversationID,
				body.Agent.RunID,
				body.Messages.UserMessageID,
			)
		}
	}

	if errors.Is(c.Request.Context().Err(), context.Canceled) {
		return
	}

	c.Header("Cache-Control", "no-store")

	if executeErr != nil {
		status, response := classifyResponseError(executeErr)
		body.Code = response.Code
		body.ErrorMessage = response.Message

		if status == http.StatusUnauthorized {
			c.Header("WWW-Authenticate", "Bearer")
		}

		c.JSON(status, body)
		return
	}

	body.ActionRequired = projectAppealAction(result.Outcome)
	c.JSON(http.StatusCreated, body)
}

func (h *MessageHandler) readSavedUserMessage(ctx context.Context, userID uint64, conversationID uint64, runID string, messageID uint64) (*model.Message, error) {
	ctx, cancel := context.WithTimeout(ctx, agentResponseReadTimeout)
	defer cancel()

	messages, err := h.service.ListByConversationID(ctx, userID, conversationID)
	if err != nil {
		return nil, service.ErrAgentAuditUnavailable
	}

	for _, message := range messages {
		if message.ID == messageID &&
			message.ConversationID == conversationID &&
			message.Role == "user" &&
			message.AgentRunID != nil &&
			*message.AgentRunID == runID {
			return &message, nil
		}
	}
	return nil, service.ErrAgentAuditUnavailable
}

func (h *MessageHandler) GetAgentRun(c *gin.Context) {
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

	runID := c.Param("run_id")
	if err := service.ValidateAppealOperationKey(runID); err != nil {
		writeError(c, err)
		return
	}

	if !h.agent || h.reads == nil {
		writeError(c, service.ErrAgentAuditConfiguration)
		return
	}

	view, readErr := h.reads.RecoverRun(c.Request.Context(), userID, conversationID, runID)

	if errors.Is(c.Request.Context().Err(), context.Canceled) {
		return
	}

	c.Header("Cache-Control", "no-store")

	if readErr != nil {
		status, response := classifyResponseError(readErr)
		if status == http.StatusUnauthorized {
			c.Header("WWW-Authenticate", "Bearer")
		}

		c.JSON(status, struct {
			ErrorResponse
			Recovery *service.AgentRecoveryView `json:"recovery,omitempty"`
		}{
			ErrorResponse: response,
			Recovery:      view,
		})
		return
	}

	if view == nil {
		writeError(c, service.ErrAgentAuditUnavailable)
		return
	}

	c.JSON(http.StatusOK, view)
}
