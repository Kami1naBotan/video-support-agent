package knowledgefailure

import (
	"errors"
	"time"
)

const MaxAttempts uint32 = 5

const (
	CodeProjectionConflict   = "projection_conflict"
	CodeProjectionLimit      = "projection_limit"
	CodeDualInputInvalid     = "dual_input_invalid"
	CodeBuildTimeout         = "build_timeout"
	CodeDualExternalFailed   = "dual_external_failed"
	CodeEpochBudgetExhausted = "epoch_budget_exhausted"

	ReasonAttemptsExhausted = "attempts_exhausted"
)

type Class string

const (
	ClassTransient Class = "transient"
	ClassPermanent Class = "permanent"
)

var ErrPolicyInput = errors.New("invalid knowledge failure policy input")

func Classify(code string) (Class, bool) {
	switch code {
	case CodeBuildTimeout, CodeDualExternalFailed:
		return ClassTransient, true
	case CodeProjectionConflict, CodeProjectionLimit, CodeDualInputInvalid, CodeEpochBudgetExhausted:
		return ClassPermanent, true
	default:
		return "", false
	}
}

type Action string

const (
	ActionRetry      Action = "retry"
	ActionDeadLetter Action = "dead_letter"
)

type Decision struct {
	Action        Action
	FailureCode   string
	ReasonCode    string
	FailedAttempt uint32
	NextAttempt   uint32
	NotBefore     time.Time
}

func retryBase(failedAttempt uint32) (time.Duration, bool) {
	switch failedAttempt {
	case 1:
		return 5 * time.Second, true
	case 2:
		return 30 * time.Second, true
	case 3:
		return 120 * time.Second, true
	case 4:
		return 300 * time.Second, true
	default:
		return 0, false
	}
}

func Plan(code string, failedAttempt uint32, now time.Time, jitterPermille uint32) (Decision, error) {
	class, known := Classify(code)
	if !known ||
		failedAttempt < 1 ||
		failedAttempt > MaxAttempts ||
		now.IsZero() ||
		now.Year() < 1 ||
		now.Year() > 9999 ||
		jitterPermille > 200 {
		return Decision{}, ErrPolicyInput
	}

	decision := Decision{
		FailureCode:   code,
		ReasonCode:    code,
		FailedAttempt: failedAttempt,
	}

	if class == ClassPermanent {
		decision.Action = ActionDeadLetter
		return decision, nil
	}

	if failedAttempt == MaxAttempts {
		decision.Action = ActionDeadLetter
		decision.ReasonCode = ReasonAttemptsExhausted
		return decision, nil
	}

	base, ok := retryBase(failedAttempt)
	if !ok {
		return Decision{}, ErrPolicyInput
	}

	jitter := base * time.Duration(jitterPermille) / 1000
	notBefore := now.UTC().Truncate(time.Millisecond).Add(base + jitter)
	if notBefore.Year() < 1 || notBefore.Year() > 9999 {
		return Decision{}, ErrPolicyInput
	}

	decision.Action = ActionRetry
	decision.NextAttempt = failedAttempt + 1
	decision.NotBefore = notBefore
	return decision, nil
}
