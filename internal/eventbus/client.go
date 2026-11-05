package eventbus

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"video-support-agent/internal/config"

	"github.com/twmb/franz-go/pkg/kgo"
)

type Client struct {
	raw       *kgo.Client
	settings  config.KafkaSettings
	cancel    context.CancelFunc
	closeOnce sync.Once
	closeErr  error
}

func NewClient(cfg config.KafkaConfig) (*Client, error) {
	return newClient(cfg)
}

func newClient(cfg config.KafkaConfig, extra ...kgo.Opt) (*Client, error) {
	settings, err := config.ParseKafkaConfig(cfg)
	if err != nil {
		return nil, err
	}

	lifetime, cancel := context.WithCancel(context.Background())

	options := []kgo.Opt{
		kgo.WithContext(lifetime),
		kgo.SeedBrokers(settings.Brokers...),
		kgo.ClientID(settings.ClientID),
		kgo.DialTimeout(settings.ConnectTimeout),
		kgo.RequestTimeoutOverhead(settings.RequestTimeout),
		kgo.RetryTimeout(settings.RequestTimeout),
		kgo.RequiredAcks(kgo.AllISRAcks()),
		kgo.ProduceRequestTimeout(settings.RequestTimeout),
		kgo.RecordDeliveryTimeout(settings.DeliveryTimeout),
		kgo.MaxBufferedRecords(1000),
		kgo.MaxBufferedBytes(8 << 20),
	}
	options = append(options, extra...)

	raw, err := kgo.NewClient(options...)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("create Kafka client: %w", err)
	}

	return &Client{
		raw:      raw,
		settings: settings,
		cancel:   cancel,
	}, nil
}

func (c *Client) Ping(ctx context.Context) error {
	bounded, cancel := context.WithTimeout(ctx, c.settings.ConnectTimeout)
	defer cancel()

	if err := c.raw.Ping(bounded); err != nil {
		return fmt.Errorf("ping Kafka: %w", err)
	}
	return nil
}

func (c *Client) Close(ctx context.Context) error {
	c.closeOnce.Do(func() {
		bounded, cancel := context.WithTimeout(ctx, c.settings.ShutdownTimeout)
		defer cancel()

		flushErr := c.raw.Flush(bounded)
		c.raw.AllowRebalance()
		leaveErr := c.raw.LeaveGroupContext(bounded)

		c.cancel()
		c.raw.Close()

		c.closeErr = errors.Join(flushErr, leaveErr)
	})

	return c.closeErr
}
