package rag

import (
	"context"
	"errors"
	"time"

	"video-support-agent/internal/llm"
	"video-support-agent/internal/model"
	"video-support-agent/internal/retrieval"

	"github.com/cloudwego/eino/schema"
	"go.uber.org/zap"
)

const (
	ResponseReserve      = 4 * time.Second
	MaxResponseWork      = 36 * time.Second
	MaxGenerationTimeout = 30 * time.Second
)

var (
	ErrResponderConfiguration = errors.New("invalid RAG responder configuration")
	ErrRetrievalFailed        = errors.New("RAG retrieval failed")
	ErrGenerationFailed       = errors.New("RAG generation failed")
)

type Responder struct {
	retriever    retrieval.HybridRetriever
	generator    llm.Generator
	modelTimeout time.Duration
	logger       *zap.Logger
}

func NewResponder(retriever retrieval.HybridRetriever, generator llm.Generator, modelTimeout time.Duration, logger *zap.Logger) (*Responder, error) {
	if retriever == nil || generator == nil || modelTimeout <= 0 || modelTimeout > MaxGenerationTimeout {
		return nil, ErrResponderConfiguration
	}

	if logger == nil {
		logger = zap.NewNop()
	}

	return &Responder{
		retriever:    retriever,
		generator:    generator,
		modelTimeout: modelTimeout,
		logger:       logger.Named("rag"),
	}, nil
}

func (r *Responder) Reply(ctx context.Context, history []llm.HistoryMessage, question string) (string, error) {
	reply, err := r.ReplyGrounded(ctx, history, question)
	if err != nil {
		return "", err
	}
	return reply.Content, nil
}

func (r *Responder) ReplyGrounded(ctx context.Context, history []llm.HistoryMessage, question string) (reply model.GroundedReply, returnErr error) {
	started := time.Now()
	report := responseReport{
		generationStatus: "not_run",
	}

	defer func() {
		if err := responseContextError(ctx); err != nil {
			returnErr = err
		}

		if returnErr != nil {
			reply = model.GroundedReply{}
		}

		if r != nil && r.logger != nil {
			r.logResponse(started, report, reply, returnErr)
		}
	}()

	if err := responseContextError(ctx); err != nil {
		return model.GroundedReply{}, err
	}

	if r == nil || r.retriever == nil || r.generator == nil || r.modelTimeout <= 0 || r.modelTimeout > MaxGenerationTimeout {
		return model.GroundedReply{}, ErrResponderConfiguration
	}

	workDeadline := started.Add(MaxResponseWork)
	if parentDeadline, ok := ctx.Deadline(); ok {
		reservedDeadline := parentDeadline.Add(-ResponseReserve)
		if reservedDeadline.Before(workDeadline) {
			workDeadline = reservedDeadline
		}
	}

	workCtx, cancelWork := context.WithDeadline(ctx, workDeadline)
	defer cancelWork()

	defer func() {
		if err := responseContextError(workCtx); err != nil {
			returnErr = err
		}
	}()

	if err := responseContextError(workCtx); err != nil {
		return model.GroundedReply{}, err
	}

	query, reason, err := PrepareQuestion(workCtx, question)
	if err != nil {
		return model.GroundedReply{}, err
	}
	if reason != FixedNone {
		report.fixedReason = reason
		return FixedReply(workCtx, reason)
	}

	if err := responseContextError(workCtx); err != nil {
		return model.GroundedReply{}, err
	}

	report.retrievalCalled = true
	retrievalStarted := time.Now()

	retrieved, err := r.retriever.Retrieve(workCtx, query, retrieval.HybridOptions{TopK: ChatTopK})

	report.retrievalDuration = time.Since(retrievalStarted)

	if contextErr := responseContextError(workCtx); contextErr != nil {
		return model.GroundedReply{}, contextErr
	}
	if err != nil {
		return model.GroundedReply{}, ErrRetrievalFailed
	}

	prepared, err := BuildContext(workCtx, history, query, retrieved, nil)
	if err != nil {
		if errors.Is(err, ErrInvalidEvidence) || errors.Is(err, ErrEvidenceLimit) {
			return model.GroundedReply{}, ErrRetrievalFailed
		}
		return model.GroundedReply{}, err
	}

	report.contextPrepared = true
	report.contextStats = prepared.Stats
	report.fixedReason = prepared.FixedReason

	if prepared.FixedReason != FixedNone {
		return FixedReply(workCtx, prepared.FixedReason)
	}

	if err := responseContextError(workCtx); err != nil {
		return model.GroundedReply{}, err
	}

	raw, err := r.generate(workCtx, prepared.Messages, &report)
	if err != nil {
		return model.GroundedReply{}, err
	}

	return ValidateAnswer(workCtx, prepared.Evidence, raw)
}

func (r *Responder) generate(ctx context.Context, messages []*schema.Message, report *responseReport) (*schema.Message, error) {
	callCtx, cancel := context.WithTimeout(ctx, r.modelTimeout)
	defer cancel()

	if err := responseContextError(callCtx); err != nil {
		report.generationStatus = generationFailureStatus(err)
		return nil, ErrGenerationFailed
	}

	report.generationCalled = true
	started := time.Now()

	raw, err := r.generator.Generate(callCtx, messages)

	report.generationDuration = time.Since(started)

	if contextErr := responseContextError(callCtx); contextErr != nil {
		report.generationStatus = generationFailureStatus(contextErr)
		return nil, ErrGenerationFailed
	}
	if err != nil {
		report.generationStatus = generationFailureStatus(err)
		return nil, ErrGenerationFailed
	}

	report.generationStatus = "returned"
	return raw, nil
}

func generationFailureStatus(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	default:
		return "failed"
	}
}

func responseContextError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	if deadline, ok := ctx.Deadline(); ok &&
		!time.Now().Before(deadline) {
		return context.DeadlineExceeded
	}

	return nil
}
