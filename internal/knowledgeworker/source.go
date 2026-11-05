package knowledgeworker

import (
	"context"
	"errors"
	"time"
	"video-support-agent/internal/knowledgeevent"

	"video-support-agent/internal/eventbus"
)

var (
	ErrSourceConfig   = errors.New("invalid source receiver configuration")
	ErrSourceRecord   = errors.New("invalid source record")
	ErrSourceConflict = errors.New("source identity conflict")
	ErrSourceState    = errors.New("source state inconsistent")
	ErrSourceStore    = errors.New("source persistence unconfirmed")
)

type SourceStore interface {
	AcceptSource(context.Context, eventbus.Delivery) error
}

func NewSourceHandler(store SourceStore) (eventbus.DurableHandler, error) {
	if store == nil {
		return nil, ErrSourceConfig
	}

	return func(ctx context.Context, delivery eventbus.Delivery) error {
		bounded, cancel := context.WithTimeout(ctx, 8*time.Second)
		defer cancel()

		if err := bounded.Err(); err != nil {
			return err
		}
		if err := store.AcceptSource(bounded, delivery); err != nil {
			return err
		}
		return bounded.Err()
	}, nil
}

func DecodeSourceDelivery(delivery eventbus.Delivery) (knowledgeevent.Source, error) {
	if delivery.Topic != eventbus.TopicDocumentEvents || delivery.Partition < 0 || delivery.Offset < 0 ||
		len(delivery.Headers) > 16 {
		return knowledgeevent.Source{}, ErrSourceRecord
	}

	messageID, ok := sourceHeader(delivery.Headers, "message_id")
	if !ok {
		return knowledgeevent.Source{}, ErrSourceRecord
	}
	traceID, ok := sourceHeader(delivery.Headers, "trace_id")
	if !ok {
		return knowledgeevent.Source{}, ErrSourceRecord
	}

	record := eventbus.Record{
		Topic:     delivery.Topic,
		Key:       string(delivery.Key),
		MessageID: messageID,
		TraceID:   traceID,
		Value:     delivery.Value,
	}
	if err := record.Validate(); err != nil {
		return knowledgeevent.Source{}, ErrSourceRecord
	}

	source, err := knowledgeevent.Decode(delivery.Value, record.Key)
	if err != nil || source.TraceID != traceID {
		return knowledgeevent.Source{}, ErrSourceRecord
	}
	return source, nil
}

func sourceHeader(headers []eventbus.Header, name string) (string, bool) {
	var value string
	found := false

	for _, header := range headers {
		if header.Key != name {
			continue
		}
		if found || len(header.Value) == 0 || len(header.Value) > 128 {
			return "", false
		}
		value = string(header.Value)
		found = true
	}

	return value, found
}
