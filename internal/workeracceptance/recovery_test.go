//go:build workerkafka

package workeracceptance

import (
	"context"
	"errors"
	"testing"
	"time"
	"video-support-agent/internal/eventbus"
	"video-support-agent/internal/knowledgeworker"
	"video-support-agent/internal/model"
	"video-support-agent/internal/repository"
)

func TestWorkerDurableOffsetsKafka(t *testing.T) {
	db, _ := workerSQLFixture(t)
	cfg, p, raw := kafka(t)
	ctx := context.Background()
	inbox, e := repository.NewKnowledgeInboxRepository(db)
	must(t, e)
	h, e := knowledgeworker.NewSourceHandler(inbox)
	must(t, e)
	g := groupAtEnd(t, raw, eventbus.TopicDocumentEvents)
	s := workerSQLSource(t, db)
	r := wireSource(t, p, s)
	entered := make(chan struct{})
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	stop, done := startConsumer(t, cfg, g, r.Topic, func(ctx context.Context, d eventbus.Delivery) error {
		if e := h(ctx, d); e != nil {
			return e
		}
		close(entered)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	select {
	case <-entered:
	case <-time.After(15 * time.Second):
		t.Fatal("not received")
	}
	workerSQLCount(t, db, &model.KnowledgeEventInbox{}, 1)
	if v := committed(t, raw, g, r.Topic, r.Partition); v != r.Offset {
		t.Fatalf("committed before handler returned: %d vs %d", v, r.Offset)
	}
	close(release)
	await(t, "durable commit", func() bool { return committed(t, raw, g, r.Topic, r.Partition) == r.Offset+1 })
	stopConsumer(t, stop, done)
	// A real persisted handler result reported as unknown must not commit; restart replays it.
	s = workerSQLSource(t, db)
	r = wireSource(t, p, s)
	_, done = startConsumer(t, cfg, g, r.Topic, func(ctx context.Context, d eventbus.Delivery) error {
		if e := h(ctx, d); e != nil {
			return e
		}
		return knowledgeworker.ErrSourceStore
	})
	select {
	case e := <-done:
		if !errors.Is(e, eventbus.ErrConsumerStore) {
			t.Fatal(e)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("failure not returned")
	}
	if committed(t, raw, g, r.Topic, r.Partition) != r.Offset {
		t.Fatal("failed handler committed")
	}
	stop, done = startConsumer(t, cfg, g, r.Topic, h)
	await(t, "replay committed", func() bool { return committed(t, raw, g, r.Topic, r.Partition) == r.Offset+1 })
	stopConsumer(t, stop, done)
	workerSQLCount(t, db, &model.KnowledgeEventInbox{}, 2)
	workerSQLCount(t, db, &model.KnowledgeIndexAttempt{}, 2)
	lag, e := p.SampleLag(ctx, g, r.Topic)
	must(t, e)
	if len(lag) != 3 {
		t.Fatal("missing partitions")
	}
	for _, v := range lag {
		if v.Status != "available" || v.Lag == nil || *v.Lag != 0 {
			t.Fatalf("lag=%+v", v)
		}
	}
	t.Logf("database persisted before commit; failure kept offset=%d; replay committed=%d; lag zero in all partitions", r.Offset, r.Offset+1)
}
