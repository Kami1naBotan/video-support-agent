package knowledgeworker

import (
	"context"
	"errors"
	"time"
	"video-support-agent/internal/knowledgeevent"
	"video-support-agent/internal/knowledgefailure"

	"video-support-agent/internal/eventbus"
)

var (
	ErrRetryConfig   = errors.New("invalid retry receiver configuration")
	ErrRetryRecord   = errors.New("invalid retry record")
	ErrRetryConflict = errors.New("retry identity conflict")
	ErrRetryState    = errors.New("retry state inconsistent")
	ErrRetryStore    = errors.New("retry persistence unconfirmed")
)

type RetryStore interface {
	AcceptRetry(context.Context, eventbus.Delivery) error
}

func NewRetryHandler(store RetryStore) (eventbus.DurableHandler, error) {
	if store == nil {
		return nil, ErrRetryConfig
	}

	return func(ctx context.Context, delivery eventbus.Delivery) error {
		bounded, cancel := context.WithTimeout(ctx, 8*time.Second)
		defer cancel()

		if err := bounded.Err(); err != nil {
			return err
		}
		if err := store.AcceptRetry(bounded, delivery); err != nil {
			return err
		}
		return bounded.Err()
	}, nil
}

func DecodeRetryDelivery(delivery eventbus.Delivery) (knowledgeevent.Recovery, error) {
	var zero knowledgeevent.Recovery

	if delivery.Topic != eventbus.TopicIndexRetry ||
		delivery.Partition < 0 ||
		delivery.Offset < 0 ||
		len(delivery.Headers) > 16 {
		return zero, ErrRetryRecord
	}

	messageID, ok := sourceHeader(delivery.Headers, "message_id")
	if !ok {
		return zero, ErrRetryRecord
	}
	traceID, ok := sourceHeader(delivery.Headers, "trace_id")
	if !ok {
		return zero, ErrRetryRecord
	}

	record := eventbus.Record{
		Topic:     delivery.Topic,
		Key:       string(delivery.Key),
		MessageID: messageID,
		TraceID:   traceID,
		Value:     delivery.Value,
	}
	if err := record.Validate(); err != nil {
		return zero, ErrRetryRecord
	}

	message, err := knowledgeevent.DecodeRecovery(delivery.Value, record.Key)
	if err != nil ||
		message.Kind != string(knowledgefailure.ActionRetry) ||
		message.MessageID != messageID ||
		message.TraceID != traceID {
		return zero, ErrRetryRecord
	}

	return message, nil
}
