package handler

import (
	"context"
	"net/http"
	"time"

	"video-support-agent/internal/service"

	"github.com/gin-gonic/gin"
)

func (h *MessageHandler) createAgentStream(c *gin.Context, userID uint64, conversationID uint64, request messageHTTPRequest) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 40*time.Second)
	defer cancel()

	stream, err := newAgentSSEWriter(c, ctx, cancel)
	if err != nil {
		writeError(c, err)
		return
	}

	started := false
	runID := ""
	resultSent := false

	writeFailure := func(err error, result *service.AgentMessageResult) {
		if stream.failed != nil || ctx.Err() != nil {
			return
		}

		status, response := classifyResponseError(err)
		body := agentStreamError{
			ErrorResponse: response,
			RunID:         runID,
		}

		if result != nil {
			body.Agent = projectAgentResult(result.Outcome)

			if result.Outcome != nil {
				if body.RunID == "" {
					body.RunID = result.Outcome.RunID
				}

				if result.Outcome.MessageSaveStatus == "saved" &&
					result.Receipt != nil &&
					result.Receipt.UserMessageID != 0 &&
					result.Receipt.AssistantMessageID != 0 {
					receipt := *result.Receipt
					body.Messages = &receipt
				}
			}
		}

		if !started {
			c.Header("Cache-Control", "no-store")
			if status == http.StatusUnauthorized {
				c.Header("WWW-Authenticate", "Bearer")
			}
			c.JSON(status, body)
			return
		}

		_ = stream.send(ctx, SSEEventError, body)
	}

	result, executeErr := h.service.CreateAgent(
		ctx,
		service.AgentMessageInput{
			UserID:         userID,
			ConversationID: conversationID,
			Content:        request.Content,
			Confirmed:      request.Confirmed,
		},
		service.AgentMessageCallbacks{
			OnStart: func() error {
				if err := ctx.Err(); err != nil {
					return err
				}

				c.Header("Content-Type", "text/event-stream; charset=utf-8")
				c.Header("Cache-Control", "no-store, no-transform")
				c.Header("X-Accel-Buffering", "no")
				c.Status(http.StatusOK)
				started = true

				return stream.send(ctx, SSEEventStart, gin.H{
					"conversation_id": conversationID,
				})
			},
			OnEvent: func(eventCtx context.Context, event service.AgentEvent) error {
				kind, payload, err := projectAgentStreamEvent(event)
				if err != nil {
					return err
				}

				if kind == SSEEventAgentStart {
					if runID != "" {
						return agentStreamProtocolError()
					}
					runID = event.RunID
				} else if runID == "" || event.RunID != runID {
					return agentStreamProtocolError()
				}

				return stream.send(eventCtx, kind, payload)
			},
			OnResult: func(eventCtx context.Context, out *service.AgentOutcome) error {
				if out == nil || runID == "" || out.RunID != runID {
					return agentStreamProtocolError()
				}

				if out.ExecutionStatus == "completed" && !out.AuditPending {
					if action := projectAppealAction(out); action != nil {
						if err := stream.send(eventCtx, SSEEventActionRequired, action); err != nil {
							return err
						}
					}
				}

				if err := stream.send(eventCtx, SSEEventAgentResult, projectAgentResult(out)); err != nil {
					return err
				}

				resultSent = true
				return nil
			},
			OnChunk: func(eventCtx context.Context, content string) error {
				if !resultSent {
					return agentStreamProtocolError()
				}
				return stream.send(eventCtx, SSEEventToken, gin.H{
					"content": content,
				})
			},
		},
	)

	if stream.failed != nil || ctx.Err() != nil {
		return
	}

	if executeErr != nil {
		writeFailure(executeErr, result)
		return
	}

	if result == nil ||
		result.Outcome == nil ||
		!resultSent ||
		runID == "" ||
		result.Outcome.RunID != runID {
		writeFailure(service.ErrAgentAuditUnavailable, result)
		return
	}

	if result.Outcome.MessageSaveStatus != "saved" ||
		result.Receipt == nil ||
		result.Receipt.UserMessageID == 0 ||
		result.Receipt.AssistantMessageID == 0 {
		writeFailure(service.ErrAgentMessageSave, result)
		return
	}

	final := projectAgentResult(result.Outcome)

	if err := stream.send(ctx, SSEEventCitation, gin.H{"items": final.Citations}); err != nil {
		return
	}

	_ = stream.send(ctx, SSEEventDone, agentStreamDone{
		RunID:              runID,
		UserMessageID:      result.Receipt.UserMessageID,
		AssistantMessageID: result.Receipt.AssistantMessageID,
		MessageSaveStatus:  "saved",
		Agent:              final,
	})
}
