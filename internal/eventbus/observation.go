package eventbus

import (
	"context"
	"errors"

	"video-support-agent/internal/observability"
)

type observedPublisher struct {
	next     Publisher
	observer *observability.AsyncObserver
}

func ObservePublisher(next Publisher, observer *observability.AsyncObserver) Publisher {
	if observer == nil {
		return next
	}
	return &observedPublisher{next: next, observer: observer}
}

func (p *observedPublisher) Publish(ctx context.Context, input Record) (receipt Receipt, returnErr error) {
	if err := input.Validate(); err != nil {
		return Receipt{}, errors.Join(ErrNotSubmitted, err)
	}
	if err := ctx.Err(); err != nil {
		return Receipt{}, errors.Join(ErrNotSubmitted, err)
	}

	ctx, finish := p.observer.Start(ctx, "publish", input.TraceParent, nil)
	outcome := "panic"
	defer func() { finish(outcome) }()

	input.TraceParent = observability.CaptureTraceParent(ctx)
	receipt, returnErr = p.next.Publish(ctx, input)
	outcome = deliveryOutcome(returnErr)
	return
}

func deliveryOutcome(err error) string {
	switch {
	case err == nil:
		return "success"
	case errors.Is(err, ErrDeliveryUnknown):
		return "unknown"
	case errors.Is(err, context.DeadlineExceeded):
		return "timed_out"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	default:
		return "failed"
	}
}

func DeliveryTraceParent(delivery Delivery) string {
	if len(delivery.Headers) > 16 {
		return ""
	}

	parent := ""
	found := false
	for _, header := range delivery.Headers {
		if header.Key != "traceparent" {
			continue
		}
		if found || len(header.Value) != 55 {
			return ""
		}
		found = true
		parent = string(header.Value)
	}
	return observability.NormalizeTraceParent(parent)
}

func ObserveHandler(next DurableHandler, observer *observability.AsyncObserver) DurableHandler {
	return func(ctx context.Context, delivery Delivery) (returnErr error) {
		ctx, finish := observer.Start(ctx, "receive", DeliveryTraceParent(delivery), nil)
		outcome := "panic"
		defer func() { finish(outcome) }()

		returnErr = next(ctx, delivery)
		outcome = deliveryOutcome(returnErr)
		return
	}
}
