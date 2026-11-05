package knowledgeevent

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
)

var ErrInvalidPoison = errors.New("invalid poison notification")

type Poison struct {
	SchemaVersion int    `json:"schema_version"`
	MessageID     string `json:"message_id"`
	IsolationID   string `json:"isolation_id"`
	TraceID       string `json:"trace_id"`

	SourceTopic string `json:"source_topic"`
	Partition   int32  `json:"partition"`
	Offset      int64  `json:"offset"`

	RecordSHA256  string `json:"record_sha256"`
	KeySHA256     string `json:"key_sha256"`
	PayloadSHA256 string `json:"payload_sha256"`
	KeyBytes      uint64 `json:"key_bytes"`
	PayloadBytes  uint64 `json:"payload_bytes"`
	HeaderCount   uint64 `json:"header_count"`

	ReasonCode string `json:"reason_code"`
	ObservedAt string `json:"observed_at"`
}

func PoisonPosition(topic string, partition int32, offset int64) string {
	value := topic + "\n" + strconv.FormatInt(int64(partition), 10) + "\n" + strconv.FormatInt(offset, 10)
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func (p Poison) Key() string {
	return "poison:" + PoisonPosition(p.SourceTopic, p.Partition, p.Offset)
}

func ValidPoisonReason(topic, code string) bool {
	switch topic {
	case "knowledge.document.events":
		return code == "source_record_invalid" ||
			code == "source_identity_conflict"
	case "knowledge.index.retry":
		return code == "retry_record_invalid" ||
			code == "retry_identity_conflict"
	default:
		return false
	}
}
func (p Poison) Validate(key string) error {
	if p.SchemaVersion != 1 ||
		!eventIDPattern.MatchString(p.MessageID) ||
		!eventIDPattern.MatchString(p.IsolationID) ||
		p.MessageID == p.IsolationID ||
		!tracePattern.MatchString(p.TraceID) ||
		p.TraceID == strings.Repeat("0", 32) ||
		p.Partition < 0 ||
		p.Offset < 0 ||
		!ValidPoisonReason(p.SourceTopic, p.ReasonCode) ||
		key != p.Key() {
		return ErrInvalidPoison
	}

	for _, value := range []string{
		p.RecordSHA256,
		p.KeySHA256,
		p.PayloadSHA256,
	} {
		if !recoveryHashPattern.MatchString(value) {
			return ErrInvalidPoison
		}
	}

	if _, err := parseRecoveryTime(p.ObservedAt); err != nil {
		return ErrInvalidPoison
	}
	return nil
}

func EncodePoison(p Poison) ([]byte, error) {
	if err := p.Validate(p.Key()); err != nil {
		return nil, err
	}
	data, err := json.Marshal(p)
	if err != nil || len(data) > MaxSourceBytes {
		return nil, ErrInvalidPoison
	}
	return data, nil
}

func DecodePoison(data []byte, key string) (Poison, error) {
	var p Poison
	if len(data) == 0 || len(data) > MaxSourceBytes {
		return p, ErrInvalidPoison
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return Poison{}, ErrInvalidPoison
	}
	if err := p.Validate(key); err != nil {
		return Poison{}, err
	}

	canonical, err := json.Marshal(p)
	if err != nil || !bytes.Equal(data, canonical) {
		return Poison{}, ErrInvalidPoison
	}
	return p, nil
}

func PoisonTime(at time.Time) string {
	return at.UTC().Format(recoveryTimeLayout)
}
