//go:build workerkafka

package workeracceptance

import (
	"context"
	"errors"
	"github.com/twmb/franz-go/pkg/kadm"

	"testing"
	"time"
	"video-support-agent/internal/eventbus"
	"video-support-agent/internal/knowledgeevent"
	"video-support-agent/internal/knowledgeworker"
	"video-support-agent/internal/model"
	"video-support-agent/internal/repository"
)

func TestWorkerCommitOutageKafka(t *testing.T) {
	db, _ := workerSQLFixture(t)
	cfg, p, raw := kafka(t)
	ctx := context.Background()
	id, e := knowledgeevent.NewID()
	must(t, e)
	partial := kadm.Offsets{}
	partial.AddOffset(eventbus.TopicDocumentEvents, 0, 0, -1)
	response, e := kadm.NewClient(raw).CommitOffsets(ctx, "partial-"+id, partial)
	must(t, e)
	must(t, response.Error())
	rows, e := p.SampleLag(ctx, "partial-"+id, eventbus.TopicDocumentEvents)
	must(t, e)
	for _, row := range rows {
		if row.Partition != 0 && (row.Status != "uncommitted" || row.Committed != nil || row.Lag != nil) {
			t.Fatalf("missing commit misrepresented: %+v", row)
		}
	}
	g := groupAtEnd(t, raw, eventbus.TopicDocumentEvents)
	inbox, e := repository.NewKnowledgeInboxRepository(db)
	must(t, e)
	h, e := knowledgeworker.NewSourceHandler(inbox)
	must(t, e)
	s := workerSQLSource(t, db)
	r := wireSource(t, p, s)
	entered := make(chan struct{})
	release := make(chan struct{})
	_, done := startConsumer(t, cfg, g, r.Topic, func(ctx context.Context, d eventbus.Delivery) error {
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
		t.Fatal("handler did not persist")
	}
	b := broker(t)
	docker(t, "pause", b)
	defer func() {
		if docker(t, "inspect", "--format", "{{.State.Paused}}", b) == "true" {
			docker(t, "unpause", b)
		}
	}()
	close(release)
	if sampled, e := p.SampleLag(ctx, g, r.Topic); !errors.Is(e, eventbus.ErrLagUnavailable) || sampled != nil {
		t.Fatalf("unavailable lag=%v err=%v", sampled, e)
	}
	select {
	case e := <-done:
		if !errors.Is(e, eventbus.ErrConsumerCommit) {
			t.Fatalf("commit unknown=%v", e)
		}
		t.Logf("commit outage returned safe classification; deadline=%t", errors.Is(e, context.DeadlineExceeded))
	case <-time.After(25 * time.Second):
		t.Fatal("commit/close not bounded")
	}
	workerSQLCount(t, db, &model.KnowledgeEventInbox{}, 1)
	docker(t, "unpause", b)
	// A request may execute once the broker resumes even if its response was not confirmed.
	at := committed(t, raw, g, r.Topic, r.Partition)
	if at != r.Offset && at != r.Offset+1 {
		t.Fatalf("unexpected post-outage offset=%d", at)
	}
	stop, next := startConsumer(t, cfg, g, r.Topic, h)
	await(t, "offset recovery", func() bool { return committed(t, raw, g, r.Topic, r.Partition) == r.Offset+1 })
	stopConsumer(t, stop, next)
	workerSQLCount(t, db, &model.KnowledgeEventInbox{}, 1)
	workerSQLCount(t, db, &model.KnowledgeIndexAttempt{}, 1)
	t.Logf("commit result unconfirmed during real broker pause; persisted event survived; broker offset after resume=%d target=%d", at, r.Offset+1)
}
