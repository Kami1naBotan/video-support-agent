package knowledgeworker

import (
	"context"
	"errors"
	"testing"
	"video-support-agent/internal/eventbus"
)

type poisonStoreProbe struct {
	calls   int
	failure error
}

func (s *poisonStoreProbe) IsolatePoison(context.Context, eventbus.Delivery, string) error {
	s.calls++
	return s.failure
}

func TestPoisonHandlerErrorBoundary(t *testing.T) {
	for _, tc := range []struct {
		name, topic string
		failure     error
		isolate     bool
	}{
		{"source_record", eventbus.TopicDocumentEvents, ErrSourceRecord, true},
		{"source_conflict", eventbus.TopicDocumentEvents, ErrSourceConflict, true},
		{"retry_record", eventbus.TopicIndexRetry, ErrRetryRecord, true},
		{"retry_conflict", eventbus.TopicIndexRetry, ErrRetryConflict, true},
		{"source_store", eventbus.TopicDocumentEvents, ErrSourceStore, false},
		{"source_state", eventbus.TopicDocumentEvents, ErrSourceState, false},
		{"retry_store", eventbus.TopicIndexRetry, ErrRetryStore, false},
		{"retry_state", eventbus.TopicIndexRetry, ErrRetryState, false},
		{"cancel", eventbus.TopicDocumentEvents, context.Canceled, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &poisonStoreProbe{}
			h, err := NewIsolatingHandler(func(context.Context, eventbus.Delivery) error { return tc.failure }, s)
			if err != nil {
				t.Fatal(err)
			}
			err = h(context.Background(), eventbus.Delivery{Topic: tc.topic})
			if tc.isolate {
				if err != nil || s.calls != 1 {
					t.Fatalf("err=%v calls=%d", err, s.calls)
				}
			} else if !errors.Is(err, tc.failure) || s.calls != 0 {
				t.Fatalf("err=%v calls=%d", err, s.calls)
			}
		})
	}
	s := &poisonStoreProbe{failure: ErrPoisonStore}
	h, _ := NewIsolatingHandler(func(context.Context, eventbus.Delivery) error { return ErrSourceRecord }, s)
	if err := h(context.Background(), eventbus.Delivery{Topic: eventbus.TopicDocumentEvents}); !errors.Is(err, ErrPoisonStore) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := h(ctx, eventbus.Delivery{Topic: eventbus.TopicDocumentEvents}); !errors.Is(err, context.Canceled) || s.calls != 1 {
		t.Fatalf("cancel err=%v calls=%d", err, s.calls)
	}
}

func TestPoisonDigestPreservesFieldBoundaries(t *testing.T) {
	a := eventbus.Delivery{Topic: eventbus.TopicDocumentEvents, Key: []byte("ab"), Value: []byte("c")}
	b := a
	b.Key = []byte("a")
	b.Value = []byte("bc")
	first, err := PoisonDescription(a, "source_record_invalid")
	if err != nil {
		t.Fatal(err)
	}
	second, err := PoisonDescription(b, "source_record_invalid")
	if err != nil {
		t.Fatal(err)
	}
	if first.RecordSHA256 == second.RecordSHA256 {
		t.Fatal("field boundaries lost")
	}
	b = a
	b.Headers = []eventbus.Header{{Key: "trace_id", Value: []byte("different")}}
	second, err = PoisonDescription(b, "source_record_invalid")
	if err != nil {
		t.Fatal(err)
	}
	if first.RecordSHA256 == second.RecordSHA256 {
		t.Fatal("headers ignored")
	}
}
