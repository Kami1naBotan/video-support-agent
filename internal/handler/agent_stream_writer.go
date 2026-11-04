package handler

import (
	"context"
	"net/http"
	"time"

	"video-support-agent/internal/llm"

	"github.com/gin-gonic/gin"
)

const agentSSEWriteTimeout = time.Second

type agentSSEWriter struct {
	context    context.Context
	cancel     context.CancelFunc
	writer     http.ResponseWriter
	controller *http.ResponseController
	deadline   time.Time
	failed     error
}

func newAgentSSEWriter(c *gin.Context, ctx context.Context, cancel context.CancelFunc) (*agentSSEWriter, error) {
	unwrapper, ok := c.Writer.(interface {
		Unwrap() http.ResponseWriter
	})
	if !ok {
		return nil, llm.ErrStreamUnsupported
	}

	deadline, ok := ctx.Deadline()
	if !ok {
		return nil, llm.ErrStreamUnsupported
	}

	controller := http.NewResponseController(unwrapper.Unwrap())
	if err := controller.SetWriteDeadline(deadline); err != nil {
		return nil, llm.ErrStreamUnsupported
	}

	return &agentSSEWriter{
		context:    ctx,
		cancel:     cancel,
		writer:     c.Writer,
		controller: controller,
		deadline:   deadline,
	}, nil
}

func (w *agentSSEWriter) fail(err error) error {
	if w.failed == nil {
		w.failed = err
		w.cancel()
	}
	return err
}

func (w *agentSSEWriter) send(eventCtx context.Context, event SSEEvent, data any) error {
	if w.failed != nil {
		return w.failed
	}
	if err := w.context.Err(); err != nil {
		return err
	}
	if err := eventCtx.Err(); err != nil {
		return err
	}

	deadline := time.Now().Add(agentSSEWriteTimeout)
	if w.deadline.Before(deadline) {
		deadline = w.deadline
	}
	if earlier, ok := eventCtx.Deadline(); ok && earlier.Before(deadline) {
		deadline = earlier
	}

	if err := w.controller.SetWriteDeadline(deadline); err != nil {
		return w.fail(err)
	}
	if err := writeSSEEvent(w.writer, event, data); err != nil {
		return w.fail(err)
	}
	if err := w.controller.Flush(); err != nil {
		return w.fail(err)
	}

	if err := w.controller.SetWriteDeadline(w.deadline); err != nil {
		return w.fail(err)
	}
	return nil
}
