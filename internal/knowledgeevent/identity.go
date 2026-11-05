package knowledgeevent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
)

type traceContextKey struct{}

func NewID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", errors.New("generate event identity failed")
	}

	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	raw := hex.EncodeToString(value[:])

	return raw[:8] + "-" + raw[8:12] + "-" + raw[12:16] + "-" + raw[16:20] + "-" + raw[20:], nil
}

func NewTraceID() (string, error) {
	for {
		var value [16]byte
		if _, err := rand.Read(value[:]); err != nil {
			return "", errors.New("generate trace identity failed")
		}

		trace := hex.EncodeToString(value[:])
		if trace != strings.Repeat("0", 32) {
			return trace, nil
		}
	}
}

func WithTrace(ctx context.Context, trace string) (context.Context, error) {
	if !tracePattern.MatchString(trace) || trace == strings.Repeat("0", 32) {
		return nil, invalid("trace_id")
	}
	return context.WithValue(ctx, traceContextKey{}, trace), nil
}

func EnsureTrace(ctx context.Context) (context.Context, string, error) {
	if trace, ok := ctx.Value(traceContextKey{}).(string); ok {
		return ctx, trace, nil
	}

	trace, err := NewTraceID()
	if err != nil {
		return nil, "", err
	}

	traced, err := WithTrace(ctx, trace)
	return traced, trace, err
}
