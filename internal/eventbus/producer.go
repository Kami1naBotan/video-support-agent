package eventbus

import (
	"context"
	"errors"
	"video-support-agent/internal/observability"

	"github.com/twmb/franz-go/pkg/kgo"
)

type Receipt struct {
	Topic     string
	Partition int32
	Offset    int64
}

type Publisher interface {
	Publish(context.Context, Record) (Receipt, error)
}

var _ Publisher = (*Client)(nil)

func (c *Client) Publish(ctx context.Context, input Record) (Receipt, error) {
	if err := input.Validate(); err != nil {
		return Receipt{}, errors.Join(ErrNotSubmitted, err)
	}

	if err := ctx.Err(); err != nil {
		return Receipt{}, errors.Join(ErrNotSubmitted, err)
	}

	bounded, cancel := context.WithTimeout(ctx, c.settings.DeliveryTimeout)
	defer cancel()

	if err := bounded.Err(); err != nil {
		return Receipt{}, errors.Join(ErrNotSubmitted, err)
	}

	record := &kgo.Record{
		Topic: input.Topic,
		Key:   []byte(input.Key),
		Value: append([]byte(nil), input.Value...),
		Headers: []kgo.RecordHeader{
			{
				Key:   "message_id",
				Value: []byte(input.MessageID),
			},
			{
				Key:   "trace_id",
				Value: []byte(input.TraceID),
			},
		},
	}

	if parent := observability.NormalizeTraceParent(input.TraceParent); parent != "" {
		record.Headers = append(record.Headers, kgo.RecordHeader{
			Key:   "traceparent",
			Value: []byte(parent),
		})
	}

	type outcome struct {
		receipt Receipt
		err     error
	}

	done := make(chan outcome, 1)

	c.raw.TryProduce(
		bounded,
		record,
		func(saved *kgo.Record, err error) {
			if err != nil {
				done <- outcome{err: ErrDeliveryUnknown}
				return
			}

			done <- outcome{
				receipt: Receipt{
					Topic:     saved.Topic,
					Partition: saved.Partition,
					Offset:    saved.Offset,
				},
			}
		},
	)

	select {
	case result := <-done:
		return result.receipt, result.err

	case <-bounded.Done():
		select {
		case result := <-done:
			return result.receipt, result.err
		default:
			return Receipt{}, errors.Join(ErrDeliveryUnknown, bounded.Err())
		}
	}
}
