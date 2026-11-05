package knowledgeworker

import (
	"context"
	"errors"
	"time"

	"video-support-agent/internal/eventbus"

	"go.uber.org/zap"
)

func NewObservedHandler(next eventbus.DurableHandler, log *zap.Logger) (eventbus.DurableHandler, error) {
	if next == nil || log == nil {
		return nil, ErrBuildConfig
	}

	return func(ctx context.Context, delivery eventbus.Delivery) error {
		started := time.Now()
		fields := receiveObservationFields(delivery, started)

		err := next(ctx, delivery)

		code := "accepted"
		switch {
		case ctx.Err() != nil:
			code = "context_ended"
		case err == nil:
		case errors.Is(err, ErrSourceRecord), errors.Is(err, ErrRetryRecord):
			code = "record_invalid"
		case errors.Is(err, ErrSourceConflict), errors.Is(err, ErrRetryConflict):
			code = "identity_conflict"
		case errors.Is(err, ErrSourceState), errors.Is(err, ErrRetryState):
			code = "state_inconsistent"
		default:
			code = "receive_unconfirmed"
		}

		fields = append(fields,
			zap.String("code", code),
			zap.Float64("receive_duration_seconds", time.Since(started).Seconds()))

		log.Info("knowledge receive", fields...)
		return err
	}, nil
}

func receiveObservationFields(delivery eventbus.Delivery, at time.Time) []zap.Field {
	topic := "unexpected"
	if delivery.Topic == eventbus.TopicDocumentEvents ||
		delivery.Topic == eventbus.TopicIndexRetry {
		topic = delivery.Topic
	}

	fields := []zap.Field{
		zap.String("topic", topic),
		zap.Int32("partition", delivery.Partition),
		zap.Int64("offset", delivery.Offset),
	}

	switch delivery.Topic {
	case eventbus.TopicDocumentEvents:
		source, err := DecodeSourceDelivery(delivery)
		if err != nil {
			return fields
		}

		fields = append(fields,
			zap.String("event_id", source.EventID),
			zap.String("trace_id", source.TraceID))

		occurredAt, err := time.Parse(time.RFC3339Nano, source.OccurredAt)
		if err == nil {
			age := at.Sub(occurredAt).Seconds()
			fields = append(fields,
				zap.Float64("event_age_seconds", age),
				zap.Bool("event_clock_ahead", age < 0))
		}

	case eventbus.TopicIndexRetry:
		retry, err := DecodeRetryDelivery(delivery)
		if err != nil {
			return fields
		}

		fields = append(fields,
			zap.String("event_id", retry.SourceEventID),
			zap.String("trace_id", retry.TraceID),
			zap.String("failed_generation_id", retry.GenerationID),
			zap.Uint32("event_attempt", retry.NextAttempt))
	}

	return fields
}
