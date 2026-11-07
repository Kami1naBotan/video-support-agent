package tools

import (
	"context"
	"errors"
	"strings"
	"sync"
	"unicode/utf8"
	"video-support-agent/internal/service"
)

const maxEvaluationCalls = 5

type evaluationTraceKey struct{}
type evaluationCallKey struct{}

type EvaluationCallIdentity struct {
	RunID    string
	CallID   string
	Sequence uint32
}

type EvaluationInvocation struct {
	Identity  EvaluationCallIdentity `json:"-"`
	Name      string                 `json:"-"`
	Arguments string                 `json:"-"`
}

type EvaluationTrace struct {
	mu      sync.Mutex
	calls   []EvaluationInvocation
	invalid bool
	closed  bool
}

func NewEvaluationTrace(ctx context.Context) (context.Context, *EvaluationTrace, error) {
	if ctx == nil {
		return nil, nil, errors.New("evaluation trace requires context")
	}
	if ctx.Value(evaluationTraceKey{}) != nil {
		return nil, nil, errors.New("evaluation trace already attached")
	}

	trace := &EvaluationTrace{
		calls: make([]EvaluationInvocation, 0, maxEvaluationCalls),
	}
	return context.WithValue(ctx, evaluationTraceKey{}, trace), trace, nil
}

func WithEvaluationCall(ctx context.Context, identity EvaluationCallIdentity) context.Context {
	if ctx == nil || ctx.Value(evaluationTraceKey{}) == nil {
		return ctx
	}
	return context.WithValue(ctx, evaluationCallKey{}, identity)
}

func (t *EvaluationTrace) Take() ([]EvaluationInvocation, error) {
	if t == nil {
		return nil, errors.New("evaluation trace is required")
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if t.closed {
		return nil, errors.New("evaluation trace already consumed")
	}
	t.closed = true

	calls := t.calls
	t.calls = nil

	if t.invalid {
		clear(calls)
		return nil, errors.New("evaluation trace is incomplete")
	}
	return calls, nil
}

func recordEvaluationInvocation(ctx context.Context, name string, raw string) {
	if ctx == nil {
		return
	}
	trace, _ := ctx.Value(evaluationTraceKey{}).(*EvaluationTrace)
	if trace == nil {
		return
	}

	trace.mu.Lock()
	defer trace.mu.Unlock()

	if trace.closed {
		trace.invalid = true
		return
	}

	identity, ok := ctx.Value(evaluationCallKey{}).(EvaluationCallIdentity)
	if !ok ||
		service.ValidateAppealOperationKey(identity.RunID) != nil ||
		service.ValidateAppealOperationKey(identity.CallID) != nil ||
		identity.Sequence == 0 ||
		identity.Sequence > maxEvaluationCalls ||
		len(raw) == 0 ||
		len(raw) > MaxArgumentBytes ||
		!utf8.ValidString(raw) ||
		len(trace.calls) >= maxEvaluationCalls {
		trace.invalid = true
		return
	}

	switch name {
	case SearchRuleName,
		GetRuleDetailName, CheckAppealEligibilityName, CreateAppealTicketName, QueryTicketStatusName:
	default:
		trace.invalid = true
		return
	}

	for _, previous := range trace.calls {
		if previous.Identity.RunID != identity.RunID ||
			previous.Identity.CallID == identity.CallID ||
			previous.Identity.Sequence >= identity.Sequence {
			trace.invalid = true
			return
		}
	}

	trace.calls = append(trace.calls, EvaluationInvocation{
		Identity:  identity,
		Name:      name,
		Arguments: strings.Clone(raw),
	})
}
