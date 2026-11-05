package knowledgeworker

import (
	"context"
	"errors"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"testing"
	"time"
	"video-support-agent/internal/eventbus"
	"video-support-agent/internal/knowledgeevent"
)

func TestObservedHandlerPreservesErrorAndRedactsInvalidIdentity(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	failure := errors.Join(ErrSourceStore, errors.New("secret database details"))
	h, err := NewObservedHandler(func(context.Context, eventbus.Delivery) error { return failure }, zap.New(core))
	if err != nil {
		t.Fatal(err)
	}
	got := h(context.Background(), eventbus.Delivery{Topic: eventbus.TopicDocumentEvents, Value: []byte("secret payload"), Headers: []eventbus.Header{{Key: "trace_id", Value: []byte("secret trace")}}})
	if got != failure {
		t.Fatal("handler error changed")
	}
	fields := logs.All()[0].ContextMap()
	if fields["code"] != "receive_unconfirmed" {
		t.Fatal(fields)
	}
	for _, key := range []string{"trace_id", "event_id", "payload", "error"} {
		if _, ok := fields[key]; ok {
			t.Fatalf("unexpected %s", key)
		}
	}
	if _, ok := fields["receive_duration_seconds"]; !ok {
		t.Fatal("missing duration")
	}
}

func TestReceiveObservationUsesSourceTimeAndReportsClockSkew(t *testing.T) {
	at := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
	source := knowledgeevent.Source{SchemaVersion: 1, EventID: "11111111-1111-4111-8111-111111111111", EventType: knowledgeevent.DraftCreated, DocumentID: "1", VersionID: "2", ContentRevision: "1", PublicationRevision: "0", OccurredAt: at.Add(time.Second).Format(time.RFC3339Nano), TraceID: "1234567890abcdef1234567890abcdef"}
	payload, err := knowledgeevent.Encode(source)
	if err != nil {
		t.Fatal(err)
	}
	delivery := eventbus.Delivery{Topic: eventbus.TopicDocumentEvents, Key: []byte("1"), Value: payload, Headers: []eventbus.Header{{Key: "message_id", Value: []byte(source.EventID)}, {Key: "trace_id", Value: []byte(source.TraceID)}}}
	core, logs := observer.New(zap.InfoLevel)
	zap.New(core).Info("sample", receiveObservationFields(delivery, at)...)
	fields := logs.All()[0].ContextMap()
	if fields["event_age_seconds"] != float64(-1) || fields["event_clock_ahead"] != true || fields["event_id"] != source.EventID {
		t.Fatal(fields)
	}
}

type observationStoreProbe struct{}

func (observationStoreProbe) Observe(context.Context) (Observation, error) {
	return Observation{}, ErrObservation
}

type observationLagProbe struct {
	calls  int
	cancel context.CancelFunc
}

func (p *observationLagProbe) SampleLag(context.Context, string, string) ([]eventbus.PartitionLag, error) {
	p.calls++
	if p.calls == 2 {
		p.cancel()
	}
	return nil, eventbus.ErrLagUnavailable
}

func TestObservationFailureDoesNotEmitZeroSnapshot(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	core, logs := observer.New(zap.InfoLevel)
	broker := &observationLagProbe{cancel: cancel}
	err := RunObservations(ctx, observationStoreProbe{}, broker, zap.New(core), []LagTarget{{Group: "source", Topic: eventbus.TopicDocumentEvents}, {Group: "retry", Topic: eventbus.TopicIndexRetry}})
	if !errors.Is(err, context.Canceled) || broker.calls != 2 {
		t.Fatalf("err=%v calls=%d", err, broker.calls)
	}
	if logs.Len() != 2 {
		t.Fatalf("logs=%d", logs.Len())
	}
	for _, entry := range logs.All() {
		fields := entry.ContextMap()
		if fields["available"] != false {
			t.Fatal(fields)
		}
		for _, key := range []string{"epoch_gap", "state_counts", "partitions"} {
			if _, ok := fields[key]; ok {
				t.Fatalf("failure emitted %s", key)
			}
		}
	}
}
