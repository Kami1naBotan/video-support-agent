package outboxrelay

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"video-support-agent/internal/eventbus"
	"video-support-agent/internal/knowledgeevent"
	"video-support-agent/internal/model"
	"video-support-agent/internal/repository"
)

type relayStoreProbe struct {
	actions     []string
	code        string
	delay       time.Duration
	failure     error
	claim       func(context.Context) (*model.EventOutbox, error)
	settledLive bool
	id, owner   string
}

func (s *relayStoreProbe) ClaimOne(ctx context.Context) (*model.EventOutbox, error) {
	return s.claim(ctx)
}
func (s *relayStoreProbe) MarkSent(ctx context.Context, id, owner string) error {
	s.actions = append(s.actions, "sent")
	s.id = id
	s.owner = owner
	s.settledLive = ctx.Err() == nil
	return s.failure
}
func (s *relayStoreProbe) MarkRetry(_ context.Context, id, owner, code string, d time.Duration) error {
	s.actions = append(s.actions, "retry")
	s.id = id
	s.owner = owner
	s.code = code
	s.delay = d
	return s.failure
}
func (s *relayStoreProbe) MarkBlocked(_ context.Context, id, owner, code string) error {
	s.actions = append(s.actions, "blocked")
	s.id = id
	s.owner = owner
	s.code = code
	return s.failure
}

type publishProbe func(context.Context, eventbus.Record) (eventbus.Receipt, error)

func (p publishProbe) Publish(c context.Context, r eventbus.Record) (eventbus.Receipt, error) {
	return p(c, r)
}
func relayFixture(t *testing.T) *model.EventOutbox {
	t.Helper()
	s := knowledgeevent.Source{SchemaVersion: 1, EventID: "11111111-1111-4111-8111-111111111111", EventType: knowledgeevent.DraftCreated, DocumentID: "1", VersionID: "2", ContentRevision: "1", PublicationRevision: "0", OccurredAt: "2026-11-05T00:00:00Z", TraceID: "1234567890abcdef1234567890abcdef"}
	b, err := knowledgeevent.Encode(s)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	owner := "OWNER_SECRET"
	until := time.Now().Add(time.Minute)
	return &model.EventOutbox{ID: 1, MessageID: "22222222-2222-4222-8222-222222222222", EventID: s.EventID, DedupKey: "source:" + s.EventID, Topic: eventbus.TopicDocumentEvents, MessageKey: "1", TraceID: s.TraceID, Payload: b, PayloadSHA256: hex.EncodeToString(sum[:]), Status: model.OutboxSending, Attempts: 1, OwnerToken: &owner, LeaseUntil: &until}
}
func TestRelaySettlementContract(t *testing.T) {
	for _, name := range []string{"ack", "unknown", "not_submitted", "ambiguous", "bad_receipt", "sent_commit_unknown", "lease_lost", "cancel_unknown", "cancel_ack"} {
		t.Run(name, func(t *testing.T) {
			row := relayFixture(t)
			store := &relayStoreProbe{}
			core, logs := observer.New(zap.InfoLevel)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if name == "sent_commit_unknown" {
				store.failure = errors.New("DATABASE_SECRET")
			}
			if name == "lease_lost" {
				store.failure = repository.ErrOutboxLeaseLost
			}
			calls := 0
			pub := publishProbe(func(c context.Context, r eventbus.Record) (eventbus.Receipt, error) {
				calls++
				if r.MessageID != row.MessageID || r.TraceID != row.TraceID || string(r.Value) != string(row.Payload) || r.Key != "1" {
					t.Fatal("identity or bytes changed")
				}
				if _, ok := c.Deadline(); !ok {
					t.Fatal("missing publish deadline")
				}
				receipt := eventbus.Receipt{Topic: r.Topic, Partition: 1, Offset: 7}
				switch name {
				case "unknown":
					return receipt, errors.New("BROKER_SECRET")
				case "not_submitted":
					return receipt, eventbus.ErrNotSubmitted
				case "ambiguous":
					return receipt, errors.Join(eventbus.ErrNotSubmitted, eventbus.ErrDeliveryUnknown)
				case "bad_receipt":
					receipt.Offset = -1
				case "cancel_unknown":
					cancel()
					return receipt, eventbus.ErrDeliveryUnknown
				case "cancel_ack":
					cancel()
				}
				return receipt, nil
			})
			r, err := New(store, pub, zap.New(core))
			if err != nil {
				t.Fatal(err)
			}
			err = r.deliver(ctx, row)
			if name == "cancel_unknown" {
				if !errors.Is(err, context.Canceled) || len(store.actions) != 0 {
					t.Fatalf("cancellation: %v %v", err, store.actions)
				}
			} else {
				if err != nil || len(store.actions) != 1 {
					t.Fatalf("settlement: %v %v", err, store.actions)
				}
				want := "sent"
				if name == "unknown" || name == "not_submitted" || name == "ambiguous" || name == "bad_receipt" {
					want = "retry"
				}
				if store.actions[0] != want || store.id != row.MessageID || store.owner != *row.OwnerToken {
					t.Fatal("wrong settlement or identity")
				}
				if want == "retry" {
					code := repository.OutboxDeliveryUnknown
					if name == "not_submitted" {
						code = repository.OutboxNotSubmitted
					}
					if store.code != code || store.delay < time.Second || store.delay > 1200*time.Millisecond {
						t.Fatal("retry contract")
					}
				}
				if want == "sent" && !store.settledLive {
					t.Fatal("ack settlement inherited cancellation")
				}
			}
			if calls != 1 {
				t.Fatal("wrong publish calls")
			}
			b, _ := json.Marshal(logs.All())
			for _, secret := range []string{"OWNER_SECRET", "DATABASE_SECRET", "BROKER_SECRET", row.PayloadSHA256, "schema_version"} {
				if strings.Contains(string(b), secret) {
					t.Fatalf("log leaked %s", secret)
				}
			}
		})
	}
}
func TestRelayRejectsInvalidPayload(t *testing.T) {
	for _, name := range []string{"hash", "event", "trace", "key", "dedup", "topic", "json"} {
		t.Run(name, func(t *testing.T) {
			row := relayFixture(t)
			switch name {
			case "hash":
				row.PayloadSHA256 = "wrong"
			case "event":
				row.EventID = "wrong"
			case "trace":
				row.TraceID = "abcdef1234567890abcdef1234567890"
			case "key":
				row.MessageKey = "2"
			case "dedup":
				row.DedupKey = "wrong"
			case "topic":
				row.Topic = eventbus.TopicIndexRetry
			case "json":
				row.Payload = []byte("invalid")
			}
			store := &relayStoreProbe{}
			r, _ := New(store, publishProbe(func(context.Context, eventbus.Record) (eventbus.Receipt, error) {
				t.Fatal("invalid row published")
				return eventbus.Receipt{}, nil
			}), zap.NewNop())
			if err := r.deliver(context.Background(), row); err != nil {
				t.Fatal(err)
			}
			want := repository.OutboxInvalidRecord
			if name == "hash" {
				want = repository.OutboxPayloadMismatch
			}
			if len(store.actions) != 1 || store.actions[0] != "blocked" || store.code != want {
				t.Fatalf("%v %s", store.actions, store.code)
			}
		})
	}
	row := relayFixture(t)
	record, code := validate(row)
	if code != "" {
		t.Fatal(code)
	}
	record.Value[0] = 'x'
	if row.Payload[0] != '{' {
		t.Fatal("payload aliases original")
	}
}
func TestRelayLifecycleAndBackoff(t *testing.T) {
	for _, attempt := range []uint64{1, 2, 3, 9, 10, ^uint64(0)} {
		base := time.Second
		for n := uint64(1); n < attempt && base < 300*time.Second; n++ {
			base = min(base*2, 300*time.Second)
		}
		for i := 0; i < 100; i++ {
			d := retryDelay(attempt)
			if d < base || d > base+base/5 || d%time.Millisecond != 0 {
				t.Fatal("backoff outside contract")
			}
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	claims := 0
	store := &relayStoreProbe{claim: func(context.Context) (*model.EventOutbox, error) { claims++; cancel(); return relayFixture(t), nil }}
	r, _ := New(store, publishProbe(func(context.Context, eventbus.Record) (eventbus.Receipt, error) {
		t.Fatal("published after claim cancellation")
		return eventbus.Receipt{}, nil
	}), zap.NewNop())
	if err := r.Run(ctx); !errors.Is(err, context.Canceled) || claims != 1 || len(store.actions) != 0 {
		t.Fatalf("run: %v", err)
	}
	if err := r.Run(context.Background()); err == nil {
		t.Fatal("second run allowed")
	}
	if err := wait(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatal("wait ignored cancellation")
	}
}
