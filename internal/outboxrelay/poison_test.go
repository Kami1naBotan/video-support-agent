package outboxrelay

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"video-support-agent/internal/eventbus"
	"video-support-agent/internal/knowledgeevent"
	"video-support-agent/internal/model"
)

func TestPoisonNoticeRelayContract(t *testing.T) {
	p := knowledgeevent.Poison{SchemaVersion: 1, MessageID: "11111111-1111-4111-8111-111111111111", IsolationID: "22222222-2222-4222-8222-222222222222", TraceID: strings.Repeat("a", 32), SourceTopic: eventbus.TopicDocumentEvents, Partition: 0, Offset: 7, RecordSHA256: strings.Repeat("b", 64), KeySHA256: strings.Repeat("c", 64), PayloadSHA256: strings.Repeat("d", 64), KeyBytes: 2, PayloadBytes: 9, HeaderCount: 1, ReasonCode: "source_record_invalid", ObservedAt: "2026-11-05T00:00:00.000Z"}
	payload, err := knowledgeevent.EncodePoison(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := knowledgeevent.DecodePoison(payload, p.Key()); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string][]byte{
		"whitespace": append([]byte(" "), payload...),
		"duplicate":  bytes.Replace(payload, []byte(`"schema_version":1`), []byte(`"schema_version":1,"schema_version":1`), 1),
		"missing":    bytes.Replace(payload, []byte(`"partition":0,`), nil, 1),
		"unknown":    bytes.Replace(payload, []byte(`"schema_version":1`), []byte(`"schema_version":1,"raw":"secret"`), 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := knowledgeevent.DecodePoison(bad, p.Key()); err == nil {
				t.Fatal("accepted noncanonical notice")
			}
		})
	}
	digest := sha256.Sum256(payload)
	row := model.EventOutbox{MessageID: p.MessageID, EventID: p.IsolationID, TraceID: p.TraceID, Topic: eventbus.TopicIndexDLQ, MessageKey: p.Key(), DedupKey: p.Key(), Payload: payload, PayloadSHA256: hex.EncodeToString(digest[:])}
	record, code := validate(&row)
	if code != "" {
		t.Fatal(code)
	}
	record.Value[0] = 'x'
	if payload[0] != '{' {
		t.Fatal("relay did not clone payload")
	}
	for _, mutate := range []func(*model.EventOutbox){
		func(r *model.EventOutbox) { r.EventID = p.MessageID },
		func(r *model.EventOutbox) { r.Topic = eventbus.TopicIndexRetry },
		func(r *model.EventOutbox) { r.DedupKey = "other" },
		func(r *model.EventOutbox) { r.PayloadSHA256 = strings.Repeat("0", 64) },
	} {
		bad := row
		mutate(&bad)
		if _, code := validate(&bad); code == "" {
			t.Fatal("accepted mismatched outbox identity")
		}
	}
}
