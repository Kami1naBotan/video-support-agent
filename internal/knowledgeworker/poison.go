package knowledgeworker

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"

	"video-support-agent/internal/eventbus"
	"video-support-agent/internal/knowledgeevent"
)

var (
	ErrPoisonConfig   = errors.New("invalid poison handler configuration")
	ErrPoisonStore    = errors.New("poison persistence unconfirmed")
	ErrPoisonConflict = errors.New("poison location conflict")
)

type PoisonStore interface {
	IsolatePoison(context.Context, eventbus.Delivery, string) error
}

func PoisonDescription(delivery eventbus.Delivery, code string) (knowledgeevent.Poison, error) {
	var zero knowledgeevent.Poison
	if delivery.Partition < 0 ||
		delivery.Offset < 0 ||
		!knowledgeevent.ValidPoisonReason(delivery.Topic, code) {
		return zero, ErrPoisonConfig
	}

	hash := sha256.New()
	put := func(value []byte) {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(value)))
		_, _ = hash.Write(length[:])
		_, _ = hash.Write(value)
	}

	put(delivery.Key)
	put(delivery.Value)

	var count [8]byte
	binary.BigEndian.PutUint64(count[:], uint64(len(delivery.Headers)))
	_, _ = hash.Write(count[:])

	for _, header := range delivery.Headers {
		put([]byte(header.Key))
		put(header.Value)
	}

	keyHash := sha256.Sum256(delivery.Key)
	payloadHash := sha256.Sum256(delivery.Value)

	return knowledgeevent.Poison{
		SchemaVersion: 1,
		SourceTopic:   delivery.Topic,
		Partition:     delivery.Partition,
		Offset:        delivery.Offset,
		RecordSHA256:  hex.EncodeToString(hash.Sum(nil)),
		KeySHA256:     hex.EncodeToString(keyHash[:]),
		PayloadSHA256: hex.EncodeToString(payloadHash[:]),
		KeyBytes:      uint64(len(delivery.Key)),
		PayloadBytes:  uint64(len(delivery.Value)),
		HeaderCount:   uint64(len(delivery.Headers)),
		ReasonCode:    code,
	}, nil
}

func poisonReason(topic string, err error) (string, bool) {
	switch topic {
	case eventbus.TopicDocumentEvents:
		if errors.Is(err, ErrSourceRecord) {
			return "source_record_invalid", true
		}
		if errors.Is(err, ErrSourceConflict) {
			return "source_identity_conflict", true
		}

	case eventbus.TopicIndexRetry:
		if errors.Is(err, ErrRetryRecord) {
			return "retry_record_invalid", true
		}
		if errors.Is(err, ErrRetryConflict) {
			return "retry_identity_conflict", true
		}
	}
	return "", false
}

func NewIsolatingHandler(next eventbus.DurableHandler, store PoisonStore) (eventbus.DurableHandler, error) {
	if next == nil || store == nil {
		return nil, ErrPoisonConfig
	}

	return func(ctx context.Context, delivery eventbus.Delivery) error {
		err := next(ctx, delivery)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}

		code, isolate := poisonReason(delivery.Topic, err)
		if !isolate {
			return err
		}

		if err := store.IsolatePoison(ctx, delivery, code); err != nil {
			return err
		}
		return ctx.Err()
	}, nil
}
