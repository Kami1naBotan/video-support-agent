package eventbus

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"regexp"
	"sync/atomic"
	"time"

	"video-support-agent/internal/config"

	"github.com/twmb/franz-go/pkg/kgo"
)

var (
	ErrConsumerConfig = errors.New("invalid consumer configuration")
	ErrConsumerUsed   = errors.New("consumer can only run once")
	ErrConsumerFetch  = errors.New("consumer fetch failed")
	ErrConsumerStore  = errors.New("consumer durable handler failed")
	ErrConsumerCommit = errors.New("consumer offset commit unconfirmed")
	ErrConsumerClosed = errors.New("consumer client closed")
)

var consumerGroupPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)

type Header struct {
	Key   string
	Value []byte
}

type Delivery struct {
	Topic     string
	Partition int32
	Offset    int64
	Key       []byte
	Value     []byte
	Headers   []Header
}

type DurableHandler func(context.Context, Delivery) error

type Consumer struct {
	cfg   config.KafkaConfig
	group string
	topic string
	used  atomic.Bool
}

func NewConsumer(cfg config.KafkaConfig, group, topic string) (*Consumer, error) {
	if !consumerGroupPattern.MatchString(group) {
		return nil, ErrConsumerConfig
	}
	switch topic {
	case TopicDocumentEvents, TopicIndexEvents, TopicIndexRetry:
	default:
		return nil, ErrConsumerConfig
	}
	if _, err := config.ParseKafkaConfig(cfg); err != nil {
		return nil, errors.Join(ErrConsumerConfig, err)
	}
	return &Consumer{cfg: cfg, group: group, topic: topic}, nil
}

func (c *Consumer) Run(ctx context.Context, handle DurableHandler) (runErr error) {
	if handle == nil {
		return ErrConsumerConfig
	}
	if !c.used.CompareAndSwap(false, true) {
		return ErrConsumerUsed
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	client, err := newClient(c.cfg,
		kgo.ConsumerGroup(c.group),
		kgo.DisableAutoCommit(),
		kgo.ConsumeTopics(c.topic),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
		kgo.BlockRebalanceOnPoll(),
		kgo.SessionTimeout(45*time.Second),
		kgo.RebalanceTimeout(60*time.Second),
		kgo.FetchMaxWait(500*time.Millisecond),
		kgo.FetchMaxBytes(1<<20),
		kgo.FetchMaxPartitionBytes(1<<20),
	)
	if err != nil {
		return err
	}
	defer func() {
		runErr = errors.Join(runErr, client.Close(context.Background()))
	}()

	if err := client.CheckTopics(ctx); err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
		return err
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := consumeOne(ctx, client, handle); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if !errors.Is(err, ErrConsumerFetch) {
				return err
			}

			timer := time.NewTimer(time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
	}
}

func consumeOne(ctx context.Context, client *Client, handle DurableHandler) error {
	fetches := client.raw.PollRecords(ctx, 1)
	defer client.raw.AllowRebalance()

	if err := ctx.Err(); err != nil {
		return err
	}
	if fetches.IsClientClosed() {
		return ErrConsumerClosed
	}

	iter := fetches.RecordIter()
	if iter.Done() {
		if len(fetches.Errors()) != 0 {
			return ErrConsumerFetch
		}
		return nil
	}
	record := iter.Next()

	storeCtx, cancelStore := context.WithTimeout(ctx, 10*time.Second)
	err := handle(storeCtx, copyDelivery(record))
	storeContextErr := storeCtx.Err()
	cancelStore()
	if err != nil {
		return fmt.Errorf("%w: %w", ErrConsumerStore, err)
	}
	if storeContextErr != nil {
		return errors.Join(ErrConsumerStore, storeContextErr)
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	commitCtx, cancelCommit := context.WithTimeout(ctx, 10*time.Second)
	defer cancelCommit()
	if err := client.raw.CommitRecords(commitCtx, record); err != nil {
		return errors.Join(ErrConsumerCommit, commitCtx.Err())
	}
	return nil
}

func copyDelivery(record *kgo.Record) Delivery {
	delivery := Delivery{
		Topic:     record.Topic,
		Partition: record.Partition,
		Offset:    record.Offset,
		Key:       bytes.Clone(record.Key),
		Value:     bytes.Clone(record.Value),
		Headers:   make([]Header, len(record.Headers)),
	}
	for i, header := range record.Headers {
		delivery.Headers[i] = Header{
			Key:   header.Key,
			Value: bytes.Clone(header.Value),
		}
	}
	return delivery
}
