package eventbus

import (
	"context"
	"fmt"

	"github.com/twmb/franz-go/pkg/kadm"
)

const (
	TopicDocumentEvents = "knowledge.document.events"
	TopicIndexEvents    = "knowledge.index.events"
	TopicIndexRetry     = "knowledge.index.retry"
	TopicIndexDLQ       = "knowledge.index.dlq"
)

func Topics() []string {
	return []string{
		TopicDocumentEvents,
		TopicIndexEvents,
		TopicIndexRetry,
		TopicIndexDLQ,
	}
}

func knownTopic(topic string) bool {
	switch topic {
	case TopicDocumentEvents,
		TopicIndexEvents,
		TopicIndexRetry,
		TopicIndexDLQ:
		return true
	default:
		return false
	}
}

func (c *Client) CheckTopics(ctx context.Context) error {
	bounded, cancel := context.WithTimeout(ctx, c.settings.RequestTimeout)
	defer cancel()

	admin := kadm.NewClient(c.raw)
	names := Topics()

	details, err := admin.ListTopics(bounded, names...)
	if err != nil {
		return fmt.Errorf("%w: metadata unavailable", ErrTopicContract)
	}

	for _, name := range names {
		detail, ok := details[name]
		if !ok || detail.Err != nil || detail.IsInternal || len(detail.Partitions) != 3 {
			return fmt.Errorf("%w: %s metadata", ErrTopicContract, name)
		}

		for id := int32(0); id < 3; id++ {
			p, ok := detail.Partitions[id]
			if !ok || p.Err != nil || p.Leader < 0 || len(p.Replicas) != 1 || len(p.ISR) != 1 || len(p.OfflineReplicas) != 0 {
				return fmt.Errorf("%w: %s partition %d", ErrTopicContract, name, id)
			}

			if p.Replicas[0] != p.Leader || p.ISR[0] != p.Leader {
				return fmt.Errorf("%w: %s replica state", ErrTopicContract, name)
			}
		}
	}

	configs, err := admin.DescribeTopicConfigs(bounded, names...)
	if err != nil {
		return fmt.Errorf("%w: configuration unavailable", ErrTopicContract)
	}

	expected := map[string]string{
		"cleanup.policy":      "delete",
		"retention.ms":        "604800000",
		"min.insync.replicas": "1",
	}

	for _, name := range names {
		resource, err := configs.On(name, nil)
		if err != nil || resource.Err != nil {
			return fmt.Errorf("%w: %s configuration", ErrTopicContract, name)
		}

		actual := make(map[string]string)
		for _, entry := range resource.Configs {
			if entry.Value != nil {
				actual[entry.Key] = *entry.Value
			}
		}

		for key, value := range expected {
			if actual[key] != value {
				return fmt.Errorf("%w: %s %s", ErrTopicContract, name, key)
			}
		}
	}

	return bounded.Err()
}
