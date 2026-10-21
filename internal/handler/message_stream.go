package handler

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"video-support-agent/internal/middleware"
	"video-support-agent/internal/service"

	"github.com/gin-gonic/gin"
)

func (h *MessageHandler) CreateStream(c *gin.Context) {
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

	ctx, cancel := context.WithCancel(c.Request.Context())
	defer cancel()

	unwrapper, ok := c.Writer.(interface{ Unwrap() http.ResponseWriter })
	if !ok {
		writeError(c, errors.New("response writer cannot be unwrapped"))
		return
	}

	controller := http.NewResponseController(unwrapper.Unwrap())

	started := false
	transportFailed := false

	send := func(event SSEEvent, data any) error {
		if err := writeSSEEvent(c.Writer, event, data); err != nil {
			transportFailed = true
			cancel()
			return err
		}

		if err := controller.Flush(); err != nil {
			transportFailed = true
			cancel()
			return err
		}

		return nil
	}

	result, err := h.service.CreateStream(ctx, userID, conversationID, request.Content, service.StreamCallbacks{
		OnStart: func() error {
			if err := ctx.Err(); err != nil {
				return err
			}

			c.Header("Content-Type", "text/event-stream; charset=utf-8")
			c.Header("Cache-Control", "no-cache, no-transform")
			c.Header("X-Accel-Buffering", "no")
			c.Status(http.StatusOK)

			started = true

			return send(SSEEventStart, gin.H{
				"conversation_id": conversationID,
			})
		},
		OnChunk: func(part string) error {
			if err := ctx.Err(); err != nil {
				return err
			}

			return send(SSEEventToken, gin.H{
				"content": part,
			})
		},
	})

	if err != nil {
		if transportFailed || errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled) {
			return
		}

		if !started {
			writeError(c, err)
			return
		}

		_, response := classifyResponseError(err)
		_ = send(SSEEventError, response)
		return
	}

	if ctx.Err() != nil {
		return
	}

	if result == nil || result.UserMessage == nil || result.AssistantMessage == nil {
		_, response := classifyResponseError(errors.New("missing saved stream result"))
		_ = send(SSEEventError, response)
		return
	}

	if err := send(SSEEventCitation, gin.H{"items": []any{}}); err != nil {
		return
	}

	if err := ctx.Err(); err != nil {
		return
	}

	if err := send(SSEEventDone, gin.H{
		"user_message_id":      result.UserMessage.ID,
		"assistant_message_id": result.AssistantMessage.ID,
	}); err != nil {
		return
	}
}
