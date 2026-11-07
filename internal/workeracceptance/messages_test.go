//go:build workerkafka

package workeracceptance

import (
	"bytes"
	"context"
	"errors"
	"github.com/twmb/franz-go/pkg/kgo"
	"go.uber.org/zap"
	"gorm.io/gorm"
	"testing"
	"time"
	"video-support-agent/internal/eventbus"
	"video-support-agent/internal/knowledgeevent"
	"video-support-agent/internal/knowledgefailure"
	"video-support-agent/internal/knowledgeworker"
	"video-support-agent/internal/model"
	"video-support-agent/internal/observability"
	"video-support-agent/internal/outboxrelay"
	"video-support-agent/internal/repository"
)

func runRelay(t *testing.T, db *gorm.DB, p eventbus.Publisher) {
	store, e := repository.NewOutboxRepository(db)
	must(t, e)
	r, e := outboxrelay.New(store, p, zap.NewNop())
	must(t, e)
	ctx, c := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	defer func() {
		c()
		select {
		case e := <-done:
			if !errors.Is(e, context.Canceled) {
				t.Error(e)
			}
		case <-time.After(10 * time.Second):
			t.Error("relay stop timeout")
		}
	}()
	await(t, "relay all sent", func() bool {
		var n int64
		must(t, db.Model(&model.EventOutbox{}).Where("status <> ?", model.OutboxSent).Count(&n).Error)
		return n == 0
	})
}
func wireRows(t *testing.T, address string, rows []model.EventOutbox) {
	partitions := map[string]map[int32]kgo.Offset{}
	for _, r := range rows {
		partitions[r.Topic] = map[int32]kgo.Offset{0: kgo.NewOffset().AtStart(), 1: kgo.NewOffset().AtStart(), 2: kgo.NewOffset().AtStart()}
	}
	raw, e := kgo.NewClient(kgo.SeedBrokers(address), kgo.ConsumePartitions(partitions))
	must(t, e)
	defer raw.Close()
	want := map[string]model.EventOutbox{}
	for _, r := range rows {
		want[r.MessageID] = r
	}
	ctx, c := context.WithTimeout(context.Background(), 15*time.Second)
	defer c()
	for len(want) > 0 && ctx.Err() == nil {
		f := raw.PollFetches(ctx)
		if len(f.Errors()) > 0 {
			t.Fatal(f.Errors())
		}
		f.EachRecord(func(r *kgo.Record) {
			headers := map[string]string{}
			for _, h := range r.Headers {
				headers[h.Key] = string(h.Value)
			}
			row, ok := want[headers["message_id"]]
			if !ok {
				return
			}
			if r.Topic != row.Topic || string(r.Key) != row.MessageKey || headers["trace_id"] != row.TraceID || !bytes.Equal(r.Value, row.Payload) {
				t.Fatal("outbox/wire mismatch")
			}
			if headers["traceparent"] != observability.NormalizeTraceParent(row.TraceParent) {
				t.Fatal("persisted/wire trace parent mismatch")
			}
			t.Logf("wire topic=%s partition=%d offset=%d message=%s", r.Topic, r.Partition, r.Offset, row.MessageID)
			delete(want, row.MessageID)
		})
	}
	if len(want) > 0 {
		t.Fatal("missing wire rows")
	}
}
func TestWorkerRecoveryMessagesKafka(t *testing.T) {
	db, repo := workerSQLFixture(t)
	cfg, p, raw := kafka(t)
	ctx := context.Background()
	g := groupAtEnd(t, raw, eventbus.TopicIndexRetry)
	h, e := knowledgeworker.NewRetryHandler(repo)
	must(t, e)
	stop, done := startConsumer(t, cfg, g, eventbus.TopicIndexRetry, h)
	defer stopConsumer(t, stop, done)
	source := workerSQLSource(t, db)
	inbox, e := repository.NewKnowledgeInboxRepository(db)
	must(t, e)
	must(t, inbox.AcceptSource(ctx, workerSQLDelivery(t, source)))
	claim, e := repo.ClaimLatest(ctx)
	must(t, e)
	if claim == nil {
		t.Fatal("no claim")
	}
	must(t, repo.RecordBuildFailure(ctx, *claim, knowledgefailure.CodeDualExternalFailed))
	_, e = repo.RecoverPending(ctx)
	must(t, e)
	runRelay(t, db, p)
	await(t, "retry persisted", func() bool {
		var n int64
		must(t, db.Model(&model.KnowledgeRetryReceipt{}).Count(&n).Error)
		return n == 1
	})
	var attempt model.KnowledgeIndexAttempt
	must(t, db.Where("attempt = 2").Take(&attempt).Error)
	var now time.Time
	must(t, db.Raw("SELECT UTC_TIMESTAMP(3)").Scan(&now).Error)
	if !now.Before(attempt.NotBefore) {
		t.Fatal("early delivery probe missed window")
	}
	early, e := repo.ClaimLatest(ctx)
	must(t, e)
	if early != nil {
		t.Fatal("early retry built")
	}
	var row model.EventOutbox
	must(t, db.Take(&row).Error)
	r, e := p.Publish(ctx, eventbus.Record{Topic: row.Topic, Key: row.MessageKey, Value: row.Payload, MessageID: row.MessageID, TraceID: row.TraceID})
	must(t, e)
	await(t, "duplicate committed", func() bool { return committed(t, raw, g, r.Topic, r.Partition) == r.Offset+1 })
	workerSQLCount(t, db, &model.KnowledgeRetryReceipt{}, 1)
	workerSQLCount(t, db, &model.KnowledgeIndexAttempt{}, 2)
	if wait := time.Until(attempt.NotBefore) + 50*time.Millisecond; wait > 0 {
		time.Sleep(wait)
	}
	next, e := repo.ClaimLatest(ctx)
	must(t, e)
	if next == nil || next.Attempt != 2 {
		t.Fatal("due retry missing")
	}
	must(t, repo.RecordBuildFailure(ctx, *next, knowledgefailure.CodeDualInputInvalid))
	_, e = repo.RecoverPending(ctx)
	must(t, e)
	runRelay(t, db, p)
	var rows []model.EventOutbox
	must(t, db.Find(&rows).Error)
	if len(rows) != 3 {
		t.Fatalf("retry+DLQ+result=%d", len(rows))
	}
	wireRows(t, cfg.Brokers, rows)
	var event model.KnowledgeEventInbox
	must(t, db.Take(&event).Error)
	if event.Status != model.KnowledgeEventDeadLettered {
		t.Fatal("missing terminal")
	}
	t.Log("retry received before due, repeated at later offset without extra attempt; permanent retry failure produced DLQ and terminal result")
}
func TestWorkerPoisonKafka(t *testing.T) {
	db, repo := workerSQLFixture(t)
	cfg, p, raw := kafka(t)
	g := groupAtEnd(t, raw, eventbus.TopicDocumentEvents)
	inbox, e := repository.NewKnowledgeInboxRepository(db)
	must(t, e)
	h, e := knowledgeworker.NewSourceHandler(inbox)
	must(t, e)
	h, e = knowledgeworker.NewIsolatingHandler(h, repo)
	must(t, e)
	// Send malformed input through raw broker API because production publisher correctly rejects it.
	bad := &kgo.Record{Topic: eventbus.TopicDocumentEvents, Key: []byte("private-key"), Value: []byte("sensitive-poison-canary"), Headers: []kgo.RecordHeader{{Key: "private-header", Value: []byte("secret-header-canary")}}}
	must(t, raw.ProduceSync(context.Background(), bad).FirstErr())
	must(t, db.Callback().Create().Before("gorm:create").Register("test:poison_store", func(tx *gorm.DB) {
		if tx.Statement.Schema != nil && tx.Statement.Schema.Table == "knowledge_poisons" {
			tx.AddError(errors.New("injected store failure"))
		}
	}))
	_, done := startConsumer(t, cfg, g, bad.Topic, h)
	select {
	case e := <-done:
		if !errors.Is(e, eventbus.ErrConsumerStore) {
			t.Fatal(e)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("poison failure did not return")
	}
	must(t, db.Callback().Create().Remove("test:poison_store"))
	workerSQLCount(t, db, &model.KnowledgePoison{}, 0)
	if committed(t, raw, g, bad.Topic, bad.Partition) != bad.Offset {
		t.Fatal("unpersisted poison committed")
	}
	stop, done := startConsumer(t, cfg, g, bad.Topic, h)
	await(t, "poison commit", func() bool { return committed(t, raw, g, bad.Topic, bad.Partition) == bad.Offset+1 })
	// A legal event behind the poison proves the partition/group can make progress.
	source := workerSQLSource(t, db)
	r := wireSource(t, p, source)
	await(t, "legal event commit", func() bool { return committed(t, raw, g, r.Topic, r.Partition) == r.Offset+1 })
	stopConsumer(t, stop, done)
	workerSQLCount(t, db, &model.KnowledgePoison{}, 1)
	workerSQLCount(t, db, &model.KnowledgeEventInbox{}, 1)
	runRelay(t, db, p)
	var rows []model.EventOutbox
	must(t, db.Find(&rows).Error)
	if len(rows) != 1 {
		t.Fatal("duplicate notice")
	}
	if bytes.Contains(rows[0].Payload, bad.Value) || bytes.Contains(rows[0].Payload, []byte("secret-header-canary")) {
		t.Fatal("poison leak")
	}
	_, e = knowledgeevent.DecodePoison(rows[0].Payload, rows[0].MessageKey)
	must(t, e)
	wireRows(t, cfg.Brokers, rows)
}
