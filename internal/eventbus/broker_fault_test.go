//go:build kafka_integration

package eventbus

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kmsg"
)

func TestBrokerFaultsAndLifecycle(t *testing.T) {
	address, cluster, container, project := os.Getenv("KAFKA_TEST_ADDRESS"), os.Getenv("KAFKA_TEST_CLUSTER"), os.Getenv("KAFKA_TEST_CONTAINER"), os.Getenv("KAFKA_TEST_PROJECT")
	if address == "" || cluster == "" || container == "" || !strings.HasPrefix(project, "kafka-contract-") {
		t.Fatal("isolated fault target required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	cfg := contractConfig()
	cfg.Brokers = address
	cfg.ConnectTimeout = "3s"
	cfg.RequestTimeout = "5s"
	cfg.DeliveryTimeout = "10s"
	cfg.ShutdownTimeout = "3s"
	client, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close(context.Background())
	meta, err := kmsg.NewPtrMetadataRequest().RequestWith(ctx, client.raw)
	if err != nil || meta.ClusterID == nil || *meta.ClusterID != cluster {
		t.Fatalf("cluster verification failed: %v", err)
	}
	admin := kadm.NewClient(client.raw)
	docker := func(action string) error {
		commandCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		owner, err := exec.CommandContext(commandCtx, "docker", "inspect", "--format", `{{index .Config.Labels "com.docker.compose.project"}}`, container).Output()
		if err != nil || strings.TrimSpace(string(owner)) != project {
			return errors.New("fault target ownership mismatch")
		}
		output, err := exec.CommandContext(commandCtx, "docker", action, container).CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s failed: %w: %s", action, err, output)
		}
		return nil
	}
	wait := func(t *testing.T, condition func() bool) {
		t.Helper()
		deadline := time.NewTimer(15 * time.Second)
		defer deadline.Stop()
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for {
			if condition() {
				return
			}
			select {
			case <-tick.C:
			case <-deadline.C:
				t.Fatal("condition deadline exceeded")
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
	}
	type running struct {
		consumer *Consumer
		stop     context.CancelFunc
		result   chan error
		ended    chan struct{}
	}
	launch := func(t *testing.T, group, id string, handler DurableHandler) running {
		t.Helper()
		cc := cfg
		cc.ClientID = id
		c, err := NewConsumer(cc, group, TopicDocumentEvents)
		if err != nil {
			t.Fatal(err)
		}
		runCtx, stop := context.WithCancel(ctx)
		r := running{c, stop, make(chan error, 1), make(chan struct{})}
		go func() { defer close(r.ended); r.result <- c.Run(runCtx, handler) }()
		t.Cleanup(func() {
			stop()
			select {
			case <-r.ended:
			case <-time.After(8 * time.Second):
				t.Error("consumer cleanup timeout")
			}
		})
		return r
	}
	finish := func(t *testing.T, r running) error {
		t.Helper()
		select {
		case err := <-r.result:
			return err
		case <-time.After(20 * time.Second):
			t.Fatal("consumer exit timeout")
			return nil
		}
	}
	committed := func(t *testing.T, group string, partition int32) int64 {
		t.Helper()
		o, err := admin.FetchOffsets(ctx, group)
		if err != nil {
			t.Fatal(err)
		}
		v, ok := o[TopicDocumentEvents][partition]
		if !ok {
			return -1
		}
		if v.Err != nil {
			t.Fatal(v.Err)
		}
		return v.At
	}
	t.Run("ConcurrentRunAndRealPartitionHandoff", func(t *testing.T) {
		group := "fault-handoff"
		handler := func(context.Context, Delivery) error { return nil }
		a := launch(t, group, "member-a", handler)
		assignments := func(count int) (map[int32]string, bool) {
			gs, err := admin.DescribeGroups(ctx, group)
			if err != nil {
				return nil, false
			}
			g := gs[group]
			if g.Err != nil || g.State != "Stable" || len(g.Members) != count {
				return nil, false
			}
			out := map[int32]string{}
			for _, m := range g.Members {
				as, ok := m.Assigned.AsConsumer()
				if !ok {
					return nil, false
				}
				for _, topic := range as.Topics {
					for _, p := range topic.Partitions {
						if _, exists := out[p]; exists {
							t.Fatal("duplicate partition ownership")
						}
						out[p] = m.ClientID
					}
				}
			}
			return out, len(out) == 3
		}
		wait(t, func() bool { _, ok := assignments(1); return ok })
		if err := a.consumer.Run(ctx, handler); !errors.Is(err, ErrConsumerUsed) {
			t.Fatalf("concurrent run accepted: %v", err)
		}
		b := launch(t, group, "member-b", handler)
		var split map[int32]string
		wait(t, func() bool { var ok bool; split, ok = assignments(2); return ok })
		ownedByB := 0
		for _, id := range split {
			if id == "member-b" {
				ownedByB++
			}
		}
		if ownedByB == 0 || ownedByB == 3 {
			t.Fatalf("partitions not shared: %v", split)
		}
		t.Logf("two-member assignment: %v", split)
		a.stop()
		if err := finish(t, a); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel result: %v", err)
		}
		wait(t, func() bool {
			all, ok := assignments(1)
			if !ok {
				return false
			}
			for _, id := range all {
				if id != "member-b" {
					return false
				}
			}
			return true
		})
		t.Log("all three partitions transferred to member-b after member-a exit")
		b.stop()
		if err := finish(t, b); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	})
	t.Run("UnreachableBrokerIsBounded", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		ln.Close()
		bad := contractConfig()
		bad.Brokers = addr
		c, err := NewClient(bad)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close(context.Background())
		started := time.Now()
		if err := c.Ping(ctx); err == nil {
			t.Fatal("unreachable ping succeeded")
		}
		if time.Since(started) > 4*time.Second {
			t.Fatal("ping timeout exceeded")
		}
		_, err = c.Publish(ctx, contractRecord())
		if !errors.Is(err, ErrDeliveryUnknown) {
			t.Fatalf("delivery classification: %v", err)
		}
	})
	t.Run("PausedBrokerMakesDeliveryUnknown", func(t *testing.T) {
		if err := client.Ping(ctx); err != nil {
			t.Fatal(err)
		}
		if err := docker("pause"); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := docker("unpause"); err != nil {
				t.Error(err)
			}
		}()
		short, stop := context.WithTimeout(ctx, 500*time.Millisecond)
		defer stop()
		_, err := client.Publish(short, contractRecord())
		if !errors.Is(err, ErrDeliveryUnknown) {
			t.Fatalf("expected unknown: %v", err)
		}
	})
	// Flush possible late delivery before the next scenario, whose topic must be empty.
	if err := client.raw.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	t.Run("CommitFailureStopsAndRedelivers", func(t *testing.T) {
		// Use a fresh key but consume every existing record; the first record is the failure target.
		record := contractRecord()
		receipt, err := client.Publish(ctx, record)
		if err != nil {
			t.Fatal(err)
		}
		group := "fault-commit"
		entered := make(chan Delivery, 1)
		release := make(chan struct{})
		r := launch(t, group, "commit-member", func(c context.Context, d Delivery) error {
			entered <- d
			select {
			case <-release:
				return nil
			case <-c.Done():
				return c.Err()
			}
		})
		var first Delivery
		select {
		case first = <-entered:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		if err := docker("pause"); err != nil {
			t.Fatal(err)
		}
		paused := true
		defer func() {
			if paused {
				if err := docker("unpause"); err != nil {
					t.Error(err)
				}
			}
		}()
		close(release)
		err = finish(t, r)
		if !errors.Is(err, ErrConsumerCommit) {
			t.Fatalf("expected commit unknown: %v", err)
		}
		if len(entered) != 0 {
			t.Fatal("consumed beyond failed commit")
		}
		if err := docker("unpause"); err != nil {
			t.Fatal(err)
		}
		paused = false
		// A paused broker may process an already-sent commit after resume. Both outcomes are legal.
		value := committed(t, group, first.Partition)
		if value != -1 && value != first.Offset+1 {
			t.Fatalf("unexpected committed position %d", value)
		}
		t.Logf("commit returned unknown; recovered broker offset=%d; source partition=%d published offset=%d", value, receipt.Partition, receipt.Offset)
		if value == first.Offset+1 {
			// Prove no skip by publishing the next record and resuming from the acknowledged position.
			if _, err := client.Publish(ctx, record); err != nil {
				t.Fatal(err)
			}
		}
		seen := make(chan Delivery, 4)
		again := launch(t, group, "recovery-member", func(c context.Context, d Delivery) error { seen <- d; <-c.Done(); return c.Err() })
		var d Delivery
		select {
		case d = <-seen:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		want := first.Offset
		if value >= 0 {
			want = value
		}
		if d.Offset != want || d.Partition != first.Partition {
			t.Fatalf("unsafe recovery got %+v want offset %d", d, want)
		}
		again.stop()
		if err := finish(t, again); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	})
	t.Run("TopicDriftAndMissingTopicRejected", func(t *testing.T) {
		value := "60000"
		responses, err := admin.AlterTopicConfigs(ctx, []kadm.AlterConfig{{Op: kadm.SetConfig, Name: "retention.ms", Value: &value}}, TopicIndexDLQ)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range responses {
			if r.Err != nil {
				t.Fatal(r.Err)
			}
		}
		if err := client.CheckTopics(ctx); !errors.Is(err, ErrTopicContract) {
			t.Fatalf("drift accepted: %v", err)
		}
		configs, err := admin.DescribeTopicConfigs(ctx, TopicIndexDLQ)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, c := range configs {
			for _, v := range c.Configs {
				if v.Key == "retention.ms" && v.Value != nil && *v.Value == value {
					found = true
				}
			}
		}
		if !found {
			t.Fatal("checker silently repaired drift")
		}
		deleted, err := admin.DeleteTopics(ctx, TopicIndexDLQ)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range deleted {
			if r.Err != nil {
				t.Fatal(r.Err)
			}
		}
		wait(t, func() bool {
			topics, err := admin.ListTopics(kadm.WithAuthorizedOps(ctx))
			if err != nil {
				return false
			}
			_, ok := topics[TopicIndexDLQ]
			return !ok
		})
		if err := client.CheckTopics(ctx); !errors.Is(err, ErrTopicContract) {
			t.Fatalf("missing topic accepted: %v", err)
		}
		// ListTopics uses the client's metadata cache, which can include a named
		// UNKNOWN_TOPIC_OR_PARTITION entry. Query the broker directly for existence.
		request := kmsg.NewPtrMetadataRequest()
		request.AllowAutoTopicCreation = false
		metadata, err := request.RequestWith(ctx, client.raw)
		if err != nil {
			t.Fatal(err)
		}
		for _, topic := range metadata.Topics {
			if topic.Topic != nil && *topic.Topic == TopicIndexDLQ {
				t.Fatalf("broker still lists deleted topic: code=%d id=%v", topic.ErrorCode, topic.TopicID)
			}
		}
		t.Log("direct broker metadata confirms topic remains absent after CheckTopics")
	})
}
