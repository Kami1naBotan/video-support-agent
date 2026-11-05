//go:build workerkafka

package workeracceptance

import (
	"bytes"
	"context"
	"errors"
	"github.com/twmb/franz-go/pkg/kgo"
	"go.uber.org/zap"
	"testing"
	"time"
	"video-support-agent/internal/eventbus"
	"video-support-agent/internal/model"
	"video-support-agent/internal/outboxrelay"
	"video-support-agent/internal/repository"
)

type hiddenAck struct{ eventbus.Publisher }

func (p hiddenAck) Publish(ctx context.Context, r eventbus.Record) (eventbus.Receipt, error) {
	_, e := p.Publisher.Publish(ctx, r)
	if e != nil {
		return eventbus.Receipt{}, e
	}
	return eventbus.Receipt{}, eventbus.ErrDeliveryUnknown
}

type retryStopStore struct {
	*repository.OutboxRepository
	stop context.CancelFunc
}

func (s retryStopStore) MarkRetry(ctx context.Context, id, owner, code string, d time.Duration) error {
	defer s.stop()
	return s.OutboxRepository.MarkRetry(ctx, id, owner, code, d)
}
func TestWorkerSendUnknownKafka(t *testing.T) {
	db, repo := workerSQLFixture(t)
	cfg, p, _ := kafka(t)
	ctx := context.Background()
	must(t, repo.IsolatePoison(ctx, eventbus.Delivery{Topic: eventbus.TopicIndexRetry, Partition: 1, Offset: 900, Value: []byte("test poison")}, "retry_record_invalid"))
	store, e := repository.NewOutboxRepository(db)
	must(t, e)
	bounded, stop := context.WithTimeout(ctx, 15*time.Second)
	defer stop()
	relay, e := outboxrelay.New(retryStopStore{store, stop}, hiddenAck{p}, zap.NewNop())
	must(t, e)
	if e = relay.Run(bounded); !errors.Is(e, context.Canceled) {
		t.Fatalf("relay stop=%v", e)
	}
	var row model.EventOutbox
	must(t, db.Take(&row).Error)
	if row.Status != model.OutboxPending || row.SafeErrorCode != repository.OutboxDeliveryUnknown || row.Attempts != 1 {
		t.Fatal("unknown send lost retry state")
	}
	// Restart the owned broker with the same isolated volume, preserving its acknowledged record.
	docker(t, "restart", broker(t))
	await(t, "broker restarted", func() bool {
		c, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		return p.Ping(c) == nil
	})
	must(t, db.Exec("UPDATE event_outboxes SET next_attempt_at=UTC_TIMESTAMP(3)-INTERVAL 1 SECOND WHERE id=?", row.ID).Error)
	runRelay(t, db, p)
	must(t, db.First(&row, row.ID).Error)
	if row.Attempts != 2 || row.Status != model.OutboxSent {
		t.Fatal("retry not recorded")
	}
	reader, e := kgo.NewClient(kgo.SeedBrokers(cfg.Brokers), kgo.ConsumePartitions(map[string]map[int32]kgo.Offset{row.Topic: {0: kgo.NewOffset().AtStart(), 1: kgo.NewOffset().AtStart(), 2: kgo.NewOffset().AtStart()}}))
	must(t, e)
	defer reader.Close()
	readCtx, c := context.WithTimeout(ctx, 15*time.Second)
	defer c()
	var found []*kgo.Record
	for len(found) < 2 && readCtx.Err() == nil {
		f := reader.PollFetches(readCtx)
		if len(f.Errors()) > 0 {
			t.Fatal(f.Errors())
		}
		f.EachRecord(func(r *kgo.Record) {
			for _, h := range r.Headers {
				if h.Key == "message_id" && string(h.Value) == row.MessageID {
					if !bytes.Equal(r.Value, row.Payload) || string(r.Key) != row.MessageKey {
						t.Fatal("retry bytes changed")
					}
					found = append(found, r)
				}
			}
		})
	}
	if len(found) != 2 || found[0].Partition != found[1].Partition || found[0].Offset >= found[1].Offset {
		t.Fatal("missing two ordered copies")
	}
	t.Logf("acknowledged record persisted through broker restart; hidden acknowledgement caused safe repeat at offsets %d,%d", found[0].Offset, found[1].Offset)
}
