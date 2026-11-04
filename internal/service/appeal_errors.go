package service

import "errors"

var (
	ErrAppealConfiguration        = errors.New("appeal service is not configured")
	ErrAppealInvalidSnapshot      = errors.New("invalid appeal eligibility snapshot")
	ErrAppealInvalidPolicy        = errors.New("invalid appeal policy")
	ErrAppealTicketNotFound       = errors.New("appeal ticket not found")
	ErrAppealIdempotencyConflict  = errors.New("appeal idempotency conflict")
	ErrAppealConfirmationRequired = errors.New("appeal confirmation required")
	ErrAppealPreconditionChanged  = errors.New("appeal precondition changed")
	ErrAppealNeedsMoreInfo        = errors.New("appeal requires more information")
	ErrAppealIneligible           = errors.New("appeal is ineligible")
	ErrAppealOutcomeUnknown       = errors.New("appeal commit outcome is unknown")
	ErrAppealRetryableStorage     = errors.New("retryable appeal storage failure")
	ErrAppealStorage              = errors.New("appeal storage failure")
)
