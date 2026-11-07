//go:build workerdual

package workeracceptance

import (
	"context"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.uber.org/zap"
	"strconv"
	"testing"
	"time"
	"video-support-agent/internal/eventbus"
	"video-support-agent/internal/knowledgeworker"
	"video-support-agent/internal/model"
	"video-support-agent/internal/observability"
	"video-support-agent/internal/repository"
)

func TestWorkerDualObservationRuntime(t *testing.T) {
	f := newDual(t)
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	defer provider.Shutdown(context.Background())
	observer, err := observability.NewAsyncObserver(&observability.Runtime{Enabled: true, Provider: provider, Registry: prometheus.NewRegistry()})
	require.NoError(t, err)
	ctx, root := provider.Tracer("acceptance").Start(context.Background(), "source")
	source := workerSQLSource(t, f.db)
	version, err := strconv.ParseUint(source.VersionID, 10, 64)
	require.NoError(t, err)
	require.NoError(t, f.db.Omit("Version").Create(&model.RuleArticle{VersionID: version, ArticleOrder: 1, SectionPath: "test", ArticleNumber: "1", Content: "Synthetic observation rule for a real dual index build."}).Error)
	store, err := repository.NewKnowledgeInboxRepository(f.db)
	require.NoError(t, err)
	handler, err := knowledgeworker.NewSourceHandler(store)
	require.NoError(t, err)
	delivery := workerSQLDelivery(t, source)
	delivery.Headers = append(delivery.Headers, eventbus.Header{Key: "traceparent", Value: []byte(observability.CaptureTraceParent(ctx))})
	root.End()
	require.NoError(t, eventbus.ObserveHandler(handler, observer)(context.Background(), delivery))
	var inbox model.KnowledgeEventInbox
	require.NoError(t, f.db.Where("event_id = ?", source.EventID).Take(&inbox).Error)
	require.NotEmpty(t, inbox.TraceParent)
	builder, err := knowledgeworker.NewDualBuilder(f.repo, controlledEmbedding{}, f.es, f.vector, f.options)
	require.NoError(t, err)
	runtime, err := knowledgeworker.NewRuntime(f.repo, builder, zap.NewNop(), observer)
	require.NoError(t, err)
	work, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runtime.RunBuilds(work) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("runtime failed to stop")
		}
	}()
	require.Eventually(t, func() bool {
		var current model.KnowledgeEventInbox
		if f.db.Where("event_id = ?", source.EventID).Take(&current).Error != nil {
			return false
		}
		return current.Status == model.KnowledgeEventApplied
	}, 75*time.Second, 100*time.Millisecond)
	require.Eventually(t, func() bool {
		for _, span := range recorder.Ended() {
			if span.Name() != "knowledge.build" {
				continue
			}
			if len(span.Links()) != 1 {
				return false
			}
			if span.Links()[0].SpanContext.TraceID() != root.SpanContext().TraceID() {
				return false
			}
			if span.SpanContext().TraceID() == root.SpanContext().TraceID() {
				return false
			}
			for _, a := range span.Attributes() {
				if string(a.Key) == "app.outcome" && a.Value.AsString() == "published" {
					return true
				}
			}
		}
		return false
	}, 5*time.Second, 50*time.Millisecond)
	t.Log("real durable receive -> actual Runtime.RunBuilds -> ES/Milvus build -> Publish -> new-root build span with persisted source link; controlled embedding, in-memory spans; Kafka transport covered separately")
}
