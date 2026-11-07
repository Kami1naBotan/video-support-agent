//go:build workerkafka

package workeracceptance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	"testing"
	"time"
	"video-support-agent/internal/eventbus"
	"video-support-agent/internal/knowledgeevent"
	"video-support-agent/internal/knowledgeworker"
	"video-support-agent/internal/model"
	"video-support-agent/internal/observability"
	"video-support-agent/internal/repository"
)

func TestWorkerObservationPropagationKafka(t *testing.T) {
	db, buildStore := workerSQLFixture(t)
	cfg, producer, raw := kafka(t)
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	defer provider.Shutdown(context.Background())
	runtime := &observability.Runtime{Enabled: true, Provider: provider, Registry: prometheus.NewRegistry()}
	observer, err := observability.NewAsyncObserver(runtime)
	must(t, err)
	ctx, root := provider.Tracer("acceptance").Start(context.Background(), "synthetic-api")
	parent := observability.CaptureTraceParent(ctx)
	// Exercise the real source-event transaction, not a manually constructed outbox.
	id, err := knowledgeevent.NewID()
	must(t, err)
	category := model.RuleCategory{Code: id, Name: "synthetic"}
	must(t, db.Create(&category).Error)
	document := model.KnowledgeDocument{DocumentKey: id}
	version := model.RuleVersion{Version: "trace", CategoryID: category.ID, ContentRevision: 1, Status: model.RuleVersionStatusDraft, Title: "synthetic", Content: "synthetic", SourceURL: "https://example.invalid/trace"}
	must(t, repository.NewKnowledgeRepository(db).CreateDraft(ctx, &document, &version, nil))
	root.End()
	var draft model.EventOutbox
	must(t, db.Where("message_key = ?", document.ID).Take(&draft).Error)
	require.Equal(t, parent, draft.TraceParent)
	source, err := knowledgeevent.Decode(draft.Payload, draft.MessageKey)
	must(t, err)
	require.Equal(t, draft.TraceID, source.TraceID)
	digest := sha256.Sum256(draft.Payload)
	require.Equal(t, hex.EncodeToString(digest[:]), draft.PayloadSHA256)
	// End/recreate relay, publisher and handler around durable state.
	group := groupAtEnd(t, raw, eventbus.TopicDocumentEvents)
	runRelay(t, db, eventbus.ObservePublisher(producer, observer))
	inboxStore, err := repository.NewKnowledgeInboxRepository(db)
	must(t, err)
	handler, err := knowledgeworker.NewSourceHandler(inboxStore)
	must(t, err)
	stop, done := startConsumer(t, cfg, group, eventbus.TopicDocumentEvents, eventbus.ObserveHandler(handler, observer))
	await(t, "persisted source", func() bool {
		var n int64
		must(t, db.Model(&model.KnowledgeEventInbox{}).Where("event_id = ?", draft.EventID).Count(&n).Error)
		return n == 1
	})
	await(t, "committed offsets", func() bool {
		rows, e := producer.SampleLag(context.Background(), group, eventbus.TopicDocumentEvents)
		if e != nil {
			return false
		}
		for _, r := range rows {
			if r.Lag == nil || *r.Lag != 0 {
				return false
			}
		}
		return true
	})
	stopConsumer(t, stop, done)
	var inbox model.KnowledgeEventInbox
	must(t, db.Where("event_id = ?", draft.EventID).Take(&inbox).Error)
	require.NotEmpty(t, inbox.TraceParent)
	require.NotEqual(t, parent, inbox.TraceParent)
	var attempt model.KnowledgeIndexAttempt
	must(t, db.Where("event_id = ? AND attempt = 1", draft.EventID).Take(&attempt).Error)
	require.Equal(t, inbox.TraceParent, attempt.TraceParent)
	// Re-send identical payload under a new producer attempt, restart consumer,
	// and prove deduplication does not overwrite the first durable span identity.
	receipt, err := eventbus.ObservePublisher(producer, observer).Publish(context.Background(), eventbus.Record{Topic: draft.Topic, Key: draft.MessageKey, MessageID: draft.MessageID, TraceID: draft.TraceID, TraceParent: draft.TraceParent, Value: draft.Payload})
	must(t, err)
	restartedStore, err := repository.NewKnowledgeInboxRepository(db)
	must(t, err)
	handler, err = knowledgeworker.NewSourceHandler(restartedStore)
	must(t, err)
	stop, done = startConsumer(t, cfg, group, eventbus.TopicDocumentEvents, eventbus.ObserveHandler(handler, observer))
	await(t, "replay committed", func() bool { return committed(t, raw, group, draft.Topic, receipt.Partition) >= receipt.Offset+1 })
	stopConsumer(t, stop, done)
	var replay model.KnowledgeEventInbox
	must(t, db.Where("event_id = ?", draft.EventID).Take(&replay).Error)
	require.Equal(t, inbox.TraceParent, replay.TraceParent)
	require.Equal(t, inbox.PayloadSHA256, replay.PayloadSHA256)
	workerSQLCount(t, db, &model.KnowledgeEventInbox{}, 1)
	// Persist another source requiring a build, then query actual bound members.
	published := workerSQLSource(t, db)
	delivery := workerSQLDelivery(t, published)
	delivery.Headers = append(delivery.Headers, eventbus.Header{Key: "traceparent", Value: []byte(parent)})
	must(t, eventbus.ObserveHandler(handler, observer)(context.Background(), delivery))
	claim, err := buildStore.ClaimLatest(context.Background())
	must(t, err)
	require.NotNil(t, claim)
	parents, truncated, err := buildStore.ReadBuildTraceParents(context.Background(), *claim)
	must(t, err)
	require.False(t, truncated)
	require.NotEmpty(t, parents)
	_, finish := observer.Start(context.Background(), "build", "", parents)
	finish("published")
	spans := recorder.Ended()
	found := false
	for _, span := range spans {
		if span.Name() == "knowledge.build" {
			found = true
			require.NotEmpty(t, span.Links())
			require.Equal(t, trace.SpanContextFromContext(ctx).TraceID(), span.Links()[0].SpanContext.TraceID())
		}
	}
	require.True(t, found)
	must(t, buildStore.Release(context.Background(), *claim))
	// Actual broker lag feeds the production Collector.
	metrics, err := knowledgeworker.NewLagMetrics(runtime, []knowledgeworker.LagTarget{{Group: group, Topic: eventbus.TopicDocumentEvents}, {Group: "trace-retry", Topic: eventbus.TopicIndexRetry}})
	must(t, err)
	rows, err := producer.SampleLag(context.Background(), group, eventbus.TopicDocumentEvents)
	must(t, err)
	metrics.Update(knowledgeworker.LagTarget{Group: group, Topic: eventbus.TopicDocumentEvents}, rows, nil, time.Now())
	families, err := runtime.Registry.Gather()
	must(t, err)
	found = false
	for _, f := range families {
		if f.GetName() == "kafka_consumer_lag" {
			found = true
			require.Len(t, f.Metric, 3)
		}
	}
	require.True(t, found)
	_ = zap.NewNop()
	t.Log("real source transaction, relay/Kafka, durable inbox+attempt, replay identity, reconstructed handlers, bound build links and broker lag passed; no actual index build claimed")
}
