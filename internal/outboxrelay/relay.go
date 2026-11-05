package outboxrelay

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math/rand/v2"
	"strings"
	"sync/atomic"
	"time"
	"video-support-agent/internal/knowledgefailure"

	"video-support-agent/internal/eventbus"
	"video-support-agent/internal/knowledgeevent"
	"video-support-agent/internal/model"
	"video-support-agent/internal/repository"

	"go.uber.org/zap"
)

type Store interface {
	ClaimOne(context.Context) (*model.EventOutbox, error)
	MarkSent(context.Context, string, string) error
	MarkRetry(context.Context, string, string, string, time.Duration) error
	MarkBlocked(context.Context, string, string, string) error
}

var _ Store = (*repository.OutboxRepository)(nil)

type Relay struct {
	store     Store
	publisher eventbus.Publisher
	logger    *zap.Logger
	started   atomic.Bool
}

func New(store Store, publisher eventbus.Publisher, logger *zap.Logger) (*Relay, error) {
	if store == nil || publisher == nil || logger == nil {
		return nil, errors.New("relay dependencies are required")
	}
	return &Relay{store: store, publisher: publisher, logger: logger}, nil
}

func (r *Relay) Run(ctx context.Context) error {
	if !r.started.CompareAndSwap(false, true) {
		return errors.New("relay already started")
	}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		row, err := r.store.ClaimOne(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}

			r.logger.Warn("outbox claim failed", zap.String("code", "claim_failed"))
			if err := wait(ctx, time.Second); err != nil {
				return err
			}
			continue
		}
		if row == nil {
			if err := wait(ctx, 500*time.Millisecond); err != nil {
				return err
			}
			continue
		}

		if err := ctx.Err(); err != nil {
			return err
		}

		if err := r.deliver(ctx, row); err != nil {
			return err
		}
	}
}

func (r *Relay) deliver(ctx context.Context, row *model.EventOutbox) error {
	if row.ID == 0 || row.MessageID == "" || row.Status != model.OutboxSending || row.OwnerToken == nil || *row.OwnerToken == "" || row.LeaseUntil == nil || row.Attempts == 0 {
		r.logger.Error("outbox claim invalid", zap.Uint64("row_id", row.ID))

		return errors.New("invalid outbox claim metadata")
	}

	owner := *row.OwnerToken
	record, code := validate(row)
	if code != "" {
		err := r.store.MarkBlocked(ctx, row.MessageID, owner, code)
		r.settlement(row.ID, "blocked", err, zap.String("reason", code))
		return nil
	}

	fields := []zap.Field{
		zap.String("message_id", record.MessageID),
		zap.String("event_id", row.EventID),
		zap.String("trace_id", record.TraceID),
		zap.Uint64("attempt", row.Attempts),
	}

	started := time.Now()
	bounded, cancel := context.WithTimeout(ctx, 60*time.Second)
	receipt, err := r.publisher.Publish(bounded, record)
	cancel()
	fields = append(fields, zap.Duration("publish_duration", time.Since(started)))

	if err == nil && (receipt.Topic != record.Topic || receipt.Partition < 0 || receipt.Partition >= 3 || receipt.Offset < 0) {
		err = eventbus.ErrDeliveryUnknown
	}
	if err == nil {
		settleCtx, settleCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer settleCancel()
		err = r.store.MarkSent(settleCtx, row.MessageID, owner)
		fields = append(fields, zap.Int32("partition", receipt.Partition), zap.Int64("offset", receipt.Offset))
		r.settlement(row.ID, "sent", err, fields...)
		return nil
	}

	if ctx.Err() != nil {
		r.logger.Info("outbox delivery interrupted", append(fields,
			zap.String("code", "shutdown_recovery"))...)
		return ctx.Err()
	}

	code = repository.OutboxDeliveryUnknown
	if errors.Is(err, eventbus.ErrNotSubmitted) && !errors.Is(err, eventbus.ErrDeliveryUnknown) {
		code = repository.OutboxNotSubmitted
	}

	delay := retryDelay(row.Attempts)
	err = r.store.MarkRetry(ctx, row.MessageID, owner, code, delay)
	fields = append(fields, zap.String("reason", code), zap.Duration("retry_delay", delay))
	r.settlement(row.ID, "pending", err, fields...)

	return nil
}

func validate(row *model.EventOutbox) (eventbus.Record, string) {
	record := eventbus.Record{
		Topic: row.Topic, Key: row.MessageKey,
		MessageID: row.MessageID, TraceID: row.TraceID, Value: row.Payload,
	}
	if record.Validate() != nil {
		return eventbus.Record{}, repository.OutboxInvalidRecord
	}

	digest := sha256.Sum256(record.Value)
	if hex.EncodeToString(digest[:]) != row.PayloadSHA256 {
		return eventbus.Record{}, repository.OutboxPayloadMismatch
	}

	switch record.Topic {
	case eventbus.TopicDocumentEvents:
		source, err := knowledgeevent.Decode(record.Value, record.Key)
		if err != nil ||
			source.EventID != row.EventID ||
			source.TraceID != record.TraceID ||
			row.DedupKey != "source:"+source.EventID {
			return eventbus.Record{}, repository.OutboxInvalidRecord
		}

	case eventbus.TopicIndexEvents:
		result, err := knowledgeevent.DecodeResult(record.Value, record.Key)
		if err != nil ||
			result.MessageID != record.MessageID ||
			result.SourceEventID != row.EventID ||
			result.TraceID != record.TraceID ||
			row.DedupKey != "terminal:"+result.SourceEventID {
			return eventbus.Record{}, repository.OutboxInvalidRecord
		}

	case eventbus.TopicIndexRetry, eventbus.TopicIndexDLQ:
		if record.Topic == eventbus.TopicIndexDLQ && strings.HasPrefix(record.Key, "poison:") {
			notice, err := knowledgeevent.DecodePoison(record.Value, record.Key)
			if err != nil ||
				notice.MessageID != record.MessageID ||
				notice.IsolationID != row.EventID ||
				notice.TraceID != record.TraceID ||
				row.DedupKey != notice.Key() {
				return eventbus.Record{}, repository.OutboxInvalidRecord
			}
			break
		}

		recovery, err := knowledgeevent.DecodeRecovery(record.Value, record.Key)
		if err != nil ||
			recovery.MessageID != record.MessageID ||
			recovery.SourceEventID != row.EventID ||
			recovery.TraceID != record.TraceID ||
			row.DedupKey != recovery.DedupKey() {
			return eventbus.Record{}, repository.OutboxInvalidRecord
		}

		expectedKind := string(knowledgefailure.ActionRetry)
		if record.Topic == eventbus.TopicIndexDLQ {
			expectedKind = string(knowledgefailure.ActionDeadLetter)
		}
		if recovery.Kind != expectedKind {
			return eventbus.Record{}, repository.OutboxInvalidRecord
		}

	default:
		return eventbus.Record{}, repository.OutboxInvalidRecord
	}

	record.Value = append([]byte(nil), record.Value...)
	return record, ""
}

func (r *Relay) settlement(id uint64, target string, err error, fields ...zap.Field) {
	fields = append(fields, zap.Uint64("row_id", id), zap.String("target", target))
	if err == nil {
		r.logger.Info("outbox state recorded", fields...)
		return
	}

	code := "persistence_unknown"
	if errors.Is(err, repository.ErrOutboxLeaseLost) {
		code = "lease_lost"
	}

	r.logger.Warn("outbox state not confirmed", append(fields, zap.String("code", code))...)
}

func retryDelay(attempt uint64) time.Duration {
	base := time.Second
	for n := uint64(1); n < attempt && base < 300*time.Second; n++ {
		base = min(2*base, 300*time.Second)
	}

	jitterMS := rand.Int64N((base / 5).Milliseconds() + 1)

	return base + time.Duration(jitterMS)*time.Millisecond
}

func wait(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
