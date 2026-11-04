package tools

import (
	"context"
	"encoding/json"
	"errors"

	"video-support-agent/internal/service"
)

const (
	MaxArgumentBytes = 16 * 1024
	MaxResultBytes   = 32 * 1024
)

var (
	ErrConfiguration  = errors.New("tool configuration invalid")
	ErrInvalidResult  = errors.New("tool result invalid")
	ErrResultTooLarge = errors.New("tool result too large")
)

type Caller struct {
	userID         uint64
	conversationID uint64
}

func NewCaller(userID, conversationID uint64) (Caller, error) {
	c := Caller{userID: userID, conversationID: conversationID}
	if err := c.validate(); err != nil {
		return Caller{}, err
	}
	return c, nil
}

func (c Caller) validate() error {
	if c.userID == 0 {
		return service.ErrUnauthorized
	}
	if c.conversationID == 0 {
		return service.ErrInvalidArgument
	}
	return nil
}

type Failure struct {
	Code  string
	cause error
}

func (e *Failure) Error() string { return e.Code }
func (e *Failure) Unwrap() error { return e.cause }

func fail(err error) error {
	if err == nil {
		return nil
	}

	code := "unavailable"
	switch {
	case errors.Is(err, service.ErrAppealOutcomeUnknown):
		code = "outcome_unknown"
	case errors.Is(err, ErrWriteAuditRequired):
		code = "audit_unavailable"
	case errors.Is(err, ErrCreateAlreadyAttempted):
		code = "create_already_attempted"
	case errors.Is(err, service.ErrAppealIdempotencyConflict):
		code = "idempotency_conflict"
	case errors.Is(err, service.ErrAppealConfirmationRequired):
		code = "confirmation_required"
	case errors.Is(err, service.ErrAppealPreconditionChanged):
		code = "precondition_changed"
	case errors.Is(err, service.ErrAppealNeedsMoreInfo):
		code = "needs_more_info"
	case errors.Is(err, service.ErrAppealIneligible):
		code = "ineligible"
	case errors.Is(err, context.DeadlineExceeded):
		code = "timed_out"
	case errors.Is(err, context.Canceled):
		code = "cancelled"
	case errors.Is(err, service.ErrUnauthorized):
		code = "unauthorized"
	case errors.Is(err, service.ErrInvalidArgument):
		code = "invalid_argument"
	case errors.Is(err, service.ErrConversationNotFound),
		errors.Is(err, service.ErrAppealTicketNotFound),
		errors.Is(err, service.ErrKnowledgeDocumentNotFound),
		errors.Is(err, service.ErrKnowledgeCurrentVersionNotFound),
		errors.Is(err, service.ErrKnowledgeVersionNotFound),
		errors.Is(err, service.ErrKnowledgeArticleNotFound):
		code = "not_found"
	case errors.Is(err, ErrConfiguration),
		errors.Is(err, service.ErrAppealConfiguration):
		code = "configuration_error"
	case errors.Is(err, ErrInvalidResult),
		errors.Is(err, service.ErrAppealInvalidSnapshot),
		errors.Is(err, service.ErrAppealInvalidPolicy):
		code = "invalid_result"
	case errors.Is(err, ErrResultTooLarge):
		code = "result_too_large"
	}

	return &Failure{Code: code, cause: err}
}

func encodeResult(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", fail(ErrInvalidResult)
	}
	if len(data) > MaxResultBytes {
		return "", fail(ErrResultTooLarge)
	}
	return string(data), nil
}
