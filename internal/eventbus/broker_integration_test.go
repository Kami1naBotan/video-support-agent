//go:build kafka_integration

package eventbus

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/kmsg"
)

func TestBrokerDeliveryAndCommitOrder(t *testing.T) {
	address, cluster := os.Getenv("KAFKA_TEST_ADDRESS"), os.Getenv("KAFKA_TEST_CLUSTER")
	if address == "" || cluster == "" || address == "127.0.0.1:19092" {
		t.Fatal("explicit isolated broker identity required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()
	cfg := contractConfig()
	cfg.Brokers = address
	cfg.ConnectTimeout = "5s"
	cfg.RequestTimeout = "10s"
	cfg.DeliveryTimeout = "20s"
	cfg.ShutdownTimeout = "5s"
	producer, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := producer.Close(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	req := kmsg.NewPtrMetadataRequest()
	resp, err := req.RequestWith(ctx, producer.raw)
	if err != nil {
		t.Fatal(err)
	}
	if resp.ClusterID == nil || *resp.ClusterID != cluster {
		t.Fatal("isolated cluster identity mismatch")
	}
	t.Logf("verified isolated cluster=%s broker=%s", cluster, address)
	if err := producer.CheckTopics(ctx); err != nil {
		t.Fatal(err)
	}

	receipts := map[string][]Receipt{}
	payloads := map[string][]Record{}
	t.Run("ConfirmedReceiptsAndWireContents", func(t *testing.T) {
		for _, topic := range Topics() {
			for i := 0; i < 3; i++ {
				r := contractRecord()
				r.Topic = topic
				r.Value = []byte(fmt.Sprintf(`{"synthetic":true,"sequence":%d}`, i))
				r.MessageID = fmt.Sprintf("12345678-1234-4234-8234-%012d", i+1)
				receipt, err := producer.Publish(ctx, r)
				if err != nil {
					t.Fatal(err)
				}
				if receipt.Topic != topic || receipt.Offset != int64(i) || receipt.Partition < 0 || receipt.Partition > 2 {
					t.Fatalf("unexpected receipt: %+v", receipt)
				}
				if i > 0 && receipt.Partition != receipts[topic][0].Partition {
					t.Fatal("same key changed partition")
				}
				receipts[topic] = append(receipts[topic], receipt)
				payloads[topic] = append(payloads[topic], r)
			}
			reader, err := kgo.NewClient(kgo.SeedBrokers(address), kgo.ConsumePartitions(map[string]map[int32]kgo.Offset{topic: {receipts[topic][0].Partition: kgo.NewOffset().AtStart()}}))
			if err != nil {
				t.Fatal(err)
			}
			func() {
				defer reader.Close()
				seen := 0
				for seen < 3 {
					fetches := reader.PollRecords(ctx, 3-seen)
					if len(fetches.Errors()) != 0 {
						t.Fatalf("fetch errors: %v", fetches.Errors())
					}
					for iter := fetches.RecordIter(); !iter.Done(); {
						actual := iter.Next()
						want := payloads[topic][seen]
						receipt := receipts[topic][seen]
						if actual.Topic != receipt.Topic || actual.Partition != receipt.Partition || actual.Offset != receipt.Offset || string(actual.Key) != want.Key || !bytes.Equal(actual.Value, want.Value) {
							t.Fatal("receipt/body mismatch")
						}
						if len(actual.Headers) != 2 || actual.Headers[0].Key != "message_id" || string(actual.Headers[0].Value) != want.MessageID || actual.Headers[1].Key != "trace_id" || string(actual.Headers[1].Value) != want.TraceID {
							t.Fatal("headers mismatch")
						}
						seen++
					}
					if ctx.Err() != nil {
						t.Fatal(ctx.Err())
					}
				}
			}()
			t.Logf("%s partition=%d offsets=0,1,2 verified", topic, receipts[topic][0].Partition)
		}
	})
	if t.Failed() {
		return
	}
	topic := TopicDocumentEvents
	partition := receipts[topic][0].Partition
	group := "isolated-commit-order"
	admin := kadm.NewClient(producer.raw)
	offset := func() int64 {
		t.Helper()
		values, err := admin.FetchOffsets(ctx, group)
		if err != nil {
			t.Fatal(err)
		}
		value, ok := values[topic][partition]
		if !ok {
			return -1
		}
		if value.Err != nil {
			t.Fatal(value.Err)
		}
		return value.At
	}
	waitOffset := func(want int64) {
		t.Helper()
		deadline := time.NewTimer(8 * time.Second)
		defer deadline.Stop()
		tick := time.NewTicker(25 * time.Millisecond)
		defer tick.Stop()
		for {
			got := offset()
			if got == want {
				t.Logf("group=%s partition=%d committed=%d", group, partition, got)
				return
			}
			if got > want {
				t.Fatalf("commit skipped: got %d want %d", got, want)
			}
			select {
			case <-tick.C:
			case <-deadline.C:
				t.Fatalf("commit did not reach %d", want)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
	}
	receive := func(ch <-chan Delivery) Delivery {
		t.Helper()
		select {
		case d := <-ch:
			return d
		case <-ctx.Done():
			t.Fatal(ctx.Err())
			return Delivery{}
		}
	}
	// Cleanup uses a separate completion channel so observing the result cannot consume its signal.

	run := func(handler DurableHandler) (context.CancelFunc, <-chan error) {
		t.Helper()
		consumer, err := NewConsumer(cfg, group, topic)
		if err != nil {
			t.Fatal(err)
		}
		runCtx, stop := context.WithCancel(ctx)
		result := make(chan error, 1)
		ended := make(chan struct{})
		go func() { defer close(ended); result <- consumer.Run(runCtx, handler) }()
		t.Cleanup(func() {
			stop()
			select {
			case <-ended:
			case <-time.After(8 * time.Second):
				t.Error("consumer did not terminate")
			}
		})
		return stop, result
	}
	sentinel := errors.New("synthetic durable failure")
	t.Run("DurableFailureDoesNotCommitOrSkip", func(t *testing.T) {
		entered := make(chan Delivery, 3)
		release := make(chan struct{})
		_, done := run(func(c context.Context, d Delivery) error {
			entered <- d
			select {
			case <-release:
				return sentinel
			case <-c.Done():
				return c.Err()
			}
		})
		first := receive(entered)
		if first.Offset != 0 {
			t.Fatalf("first offset=%d", first.Offset)
		}
		for i := 0; i < 3; i++ {
			if got := offset(); got != -1 {
				t.Fatalf("committed before durable success: %d", got)
			}
		}
		close(release)
		select {
		case err := <-done:
			if !errors.Is(err, ErrConsumerStore) || !errors.Is(err, sentinel) {
				t.Fatalf("wrong error: %v", err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		if got := offset(); got != -1 {
			t.Fatalf("failed record committed: %d", got)
		}
		if len(entered) != 0 {
			t.Fatal("consumed past failed record")
		}
		t.Log("offset 0 failed; group has no committed offset; later records not handled")
	})
	if t.Failed() {
		return
	}
	t.Run("SameGroupRedeliveryAndOrderedCommit", func(t *testing.T) {
		received := make(chan Delivery, 3)
		stop, done := run(func(c context.Context, d Delivery) error {
			received <- d
			if d.Offset == 2 {
				<-c.Done()
				return c.Err()
			}
			return nil
		})
		for i := int64(0); i < 3; i++ {
			d := receive(received)
			if d.Offset != i {
				t.Fatalf("expected offset %d got %d", i, d.Offset)
			}
		}
		waitOffset(2)
		stop()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("expected cancel: %v", err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		if got := offset(); got != 2 {
			t.Fatalf("unfinished record advanced commit: %d", got)
		}
	})
	if t.Failed() {
		return
	}
	t.Run("RestartResumesAtCommittedPosition", func(t *testing.T) {
		received := make(chan Delivery, 3)
		stop, done := run(func(_ context.Context, d Delivery) error { received <- d; return nil })
		d := receive(received)
		if d.Offset != 2 {
			t.Fatalf("restart expected 2 got %d", d.Offset)
		}
		waitOffset(3)
		stop()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("expected cancel: %v", err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		if len(received) != 0 {
			t.Fatal("unexpected extra delivery")
		}
	})
}
