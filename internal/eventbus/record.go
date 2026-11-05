package eventbus

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

const MaxPayloadBytes = 64 << 10

var (
	ErrRecordInvalid   = errors.New("invalid Kafka record")
	ErrNotSubmitted    = errors.New("Kafka record not submitted")
	ErrDeliveryUnknown = errors.New("Kafka delivery unknown")
	ErrTopicContract   = errors.New("Kafka topic contract mismatch")

	messageIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	traceIDPattern   = regexp.MustCompile(`^[0-9a-f]{32}$`)
	poisonKeyPattern = regexp.MustCompile(`^poison:[0-9a-f]{64}$`)
)

type Record struct {
	Topic     string
	Key       string
	MessageID string
	TraceID   string
	Value     []byte
}

func (r Record) Validate() error {
	bad := func(field string) error {
		return fmt.Errorf("%w: %s", ErrRecordInvalid, field)
	}

	if !knownTopic(r.Topic) {
		return bad("topic")
	}

	id, err := strconv.ParseUint(r.Key, 10, 64)
	documentKey := err == nil && id > 0 &&
		strconv.FormatUint(id, 10) == r.Key

	if !documentKey && !(r.Topic == TopicIndexDLQ && poisonKeyPattern.MatchString(r.Key)) {
		return bad("key")
	}

	if !messageIDPattern.MatchString(r.MessageID) {
		return bad("message_id")
	}

	if !traceIDPattern.MatchString(r.TraceID) || r.TraceID == strings.Repeat("0", 32) {
		return bad("trace_id")
	}

	if len(r.Value) == 0 || len(r.Value) > MaxPayloadBytes || !utf8.Valid(r.Value) {
		return bad("value size or encoding")
	}

	body := bytes.TrimSpace(r.Value)
	if len(body) == 0 || body[0] != '{' || !json.Valid(body) {
		return bad("JSON object")
	}

	return nil
}
