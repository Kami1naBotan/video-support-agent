package eventbus

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/twmb/franz-go/pkg/kgo"
	"video-support-agent/internal/config"
)

func contractConfig() config.KafkaConfig {
	return config.KafkaConfig{Brokers: "127.0.0.1:1", ClientID: "contract-test", ConnectTimeout: "1s", RequestTimeout: "1s", DeliveryTimeout: "2s", ShutdownTimeout: "1s"}
}
func contractRecord() Record {
	return Record{Topic: TopicDocumentEvents, Key: "1", MessageID: "12345678-1234-4234-8234-123456789abc", TraceID: "123456789abcdef0123456789abcdef0", Value: []byte(`{"test":true}`)}
}
func TestRecordContract(t *testing.T) {
	for _, topic := range Topics() {
		t.Run(topic, func(t *testing.T) {
			r := contractRecord()
			r.Topic = topic
			if err := r.Validate(); err != nil {
				t.Fatal(err)
			}
		})
	}
	cases := map[string]func(*Record){
		"unknown_topic":      func(r *Record) { r.Topic = "secret" },
		"zero_key":           func(r *Record) { r.Key = "0" },
		"negative_key":       func(r *Record) { r.Key = "-1" },
		"leading_zero":       func(r *Record) { r.Key = "01" },
		"overflow":           func(r *Record) { r.Key = "18446744073709551616" },
		"poison_wrong_topic": func(r *Record) { r.Key = "poison:" + strings.Repeat("a", 64) },
		"bad_uuid_version":   func(r *Record) { r.MessageID = "12345678-1234-1234-8234-123456789abc" },
		"bad_uuid_variant":   func(r *Record) { r.MessageID = "12345678-1234-4234-1234-123456789abc" },
		"uppercase_uuid":     func(r *Record) { r.MessageID = strings.ToUpper(r.MessageID) },
		"zero_trace":         func(r *Record) { r.TraceID = strings.Repeat("0", 32) },
		"short_trace":        func(r *Record) { r.TraceID = "abc" },
		"empty_payload":      func(r *Record) { r.Value = nil },
		"invalid_utf8":       func(r *Record) { r.Value = []byte{'{', 255, '}'} },
		"array":              func(r *Record) { r.Value = []byte(`[]`) },
		"null":               func(r *Record) { r.Value = []byte(`null`) },
		"malformed":          func(r *Record) { r.Value = []byte(`{"secret":`) },
		"multiple_objects":   func(r *Record) { r.Value = []byte(`{} {}`) },
		"oversize":           func(r *Record) { r.Value = []byte(`{"x":"` + strings.Repeat("a", MaxPayloadBytes-7) + `"}`) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r := contractRecord()
			mutate(&r)
			err := r.Validate()
			if !errors.Is(err, ErrRecordInvalid) {
				t.Fatalf("expected invalid record, got %v", err)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatal("payload leaked")
			}
		})
	}
	t.Run("exact_size", func(t *testing.T) {
		r := contractRecord()
		r.Value = []byte(`{"x":"` + strings.Repeat("a", MaxPayloadBytes-8) + `"}`)
		if len(r.Value) != MaxPayloadBytes {
			t.Fatal("bad fixture")
		}
		if err := r.Validate(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("poison_dlq", func(t *testing.T) {
		r := contractRecord()
		r.Topic = TopicIndexDLQ
		r.Key = "poison:" + strings.Repeat("a", 64)
		if err := r.Validate(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("max_document_id", func(t *testing.T) {
		r := contractRecord()
		r.Key = "18446744073709551615"
		if err := r.Validate(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestDeliveryPreservesUntrustedInputAndIsolation(t *testing.T) {
	original := &kgo.Record{Topic: TopicDocumentEvents, Partition: 2, Offset: 12, Key: []byte("key"), Value: []byte{255}, Headers: []kgo.RecordHeader{{Key: "trace_id", Value: []byte("first")}, {Key: "trace_id", Value: []byte("second")}}}
	got := copyDelivery(original)
	if got.Topic != original.Topic || got.Partition != 2 || got.Offset != 12 || len(got.Headers) != 2 || got.Headers[0].Key != "trace_id" || got.Headers[1].Key != "trace_id" || string(got.Headers[1].Value) != "second" || got.Value[0] != 255 {
		t.Fatal("input was lost or normalized")
	}
	got.Key[0] = 'X'
	got.Value[0] = 0
	got.Headers[0].Value[0] = 'X'
	got.Headers[1].Key = "changed"
	got.Offset = 999
	if string(original.Key) != "key" || original.Value[0] != 255 || string(original.Headers[0].Value) != "first" || original.Headers[1].Key != "trace_id" || original.Offset != 12 {
		t.Fatal("handler mutation changed source record")
	}
}

func TestTopicsReturnsIndependentContract(t *testing.T) {
	got := Topics()
	if len(got) != 4 {
		t.Fatal("expected four topics")
	}
	got[0] = "changed"
	if Topics()[0] != TopicDocumentEvents {
		t.Fatal("caller mutated shared topic list")
	}
}

func TestPublishRejectsBeforeClientAccess(t *testing.T) {
	// A nil receiver makes accidental access to the network client fail this test.
	var client *Client
	invalid := contractRecord()
	invalid.Key = "0"
	_, err := client.Publish(context.Background(), invalid)
	if !errors.Is(err, ErrNotSubmitted) || !errors.Is(err, ErrRecordInvalid) {
		t.Fatalf("unexpected rejection: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = client.Publish(ctx, contractRecord())
	if !errors.Is(err, ErrNotSubmitted) || !errors.Is(err, context.Canceled) || errors.Is(err, ErrDeliveryUnknown) {
		t.Fatalf("wrong canceled classification: %v", err)
	}
}

func TestConsumerLocalLifecycle(t *testing.T) {
	for _, group := range []string{"", "bad group", strings.Repeat("a", 129)} {
		if _, err := NewConsumer(contractConfig(), group, TopicDocumentEvents); !errors.Is(err, ErrConsumerConfig) {
			t.Fatalf("group accepted: %q", group)
		}
	}
	for _, topic := range []string{TopicIndexDLQ, "unknown"} {
		if _, err := NewConsumer(contractConfig(), "test-group", topic); !errors.Is(err, ErrConsumerConfig) {
			t.Fatal("invalid input topic accepted")
		}
	}
	for _, topic := range []string{TopicDocumentEvents, TopicIndexEvents, TopicIndexRetry} {
		if _, err := NewConsumer(contractConfig(), "test-group", topic); err != nil {
			t.Fatal(err)
		}
	}
	cfg := contractConfig()
	cfg.ConnectTimeout = "bad"
	if _, err := NewConsumer(cfg, "test-group", TopicDocumentEvents); !errors.Is(err, ErrConsumerConfig) {
		t.Fatal("invalid config accepted")
	}
	consumer, err := NewConsumer(contractConfig(), "test-group", TopicDocumentEvents)
	if err != nil {
		t.Fatal(err)
	}
	if err := consumer.Run(context.Background(), nil); !errors.Is(err, ErrConsumerConfig) {
		t.Fatal("nil handler accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	handler := func(context.Context, Delivery) error { called = true; return nil }
	if err := consumer.Run(ctx, handler); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation: %v", err)
	}
	if called {
		t.Fatal("handler ran after cancellation")
	}
	if err := consumer.Run(ctx, handler); !errors.Is(err, ErrConsumerUsed) {
		t.Fatalf("consumer reused: %v", err)
	}
}

func TestClientConstructionWithoutGroup(t *testing.T) {
	client, err := NewClient(contractConfig())
	if err != nil {
		t.Fatalf("producer-only client must construct without consumer-group options: %v", err)
	}
	if err := client.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}
