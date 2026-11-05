package knowledgeevent

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
	"video-support-agent/internal/knowledgefailure"
)

var ErrInvalidRecovery = errors.New("invalid knowledge recovery event")

var recoveryHashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

const recoveryTimeLayout = "2006-01-02T15:04:05.000Z"

type Recovery struct {
	SchemaVersion int    `json:"schema_version"`
	MessageID     string `json:"message_id"`
	SourceEventID string `json:"source_event_id"`
	DocumentID    string `json:"document_id"`
	SourceSHA256  string `json:"source_sha256"`
	GenerationID  string `json:"generation_id"`
	SourceEpoch   string `json:"source_epoch"`

	Kind           string `json:"kind"`
	FailedAttempt  uint32 `json:"failed_attempt"`
	NextAttempt    uint32 `json:"next_attempt"`
	FailureCode    string `json:"failure_code"`
	ReasonCode     string `json:"reason_code"`
	JitterPermille uint32 `json:"jitter_permille"`
	DecidedAt      string `json:"decided_at"`
	NotBefore      string `json:"not_before"`
	TraceID        string `json:"trace_id"`
}

func parseRecoveryTime(value string) (time.Time, error) {
	at, err := time.Parse(recoveryTimeLayout, value)
	if err != nil || at.IsZero() || at.Year() < 1 || at.Year() > 9999 || at.UTC().Format(recoveryTimeLayout) != value {
		return time.Time{}, ErrInvalidRecovery
	}
	return at.UTC(), nil
}

func (r Recovery) Validate(key string) error {
	if r.SchemaVersion != 1 ||
		!eventIDPattern.MatchString(r.MessageID) ||
		!eventIDPattern.MatchString(r.SourceEventID) ||
		r.MessageID == r.SourceEventID ||
		!eventIDPattern.MatchString(r.GenerationID) ||
		!canonicalUint(r.DocumentID, false) ||
		key != r.DocumentID ||
		!canonicalUint(r.SourceEpoch, false) ||
		!recoveryHashPattern.MatchString(r.SourceSHA256) ||
		!tracePattern.MatchString(r.TraceID) ||
		r.TraceID == strings.Repeat("0", 32) {
		return ErrInvalidRecovery
	}

	decidedAt, err := parseRecoveryTime(r.DecidedAt)
	if err != nil {
		return err
	}

	decision, err := knowledgefailure.Plan(r.FailureCode, r.FailedAttempt, decidedAt, r.JitterPermille)
	if err != nil || r.Kind != string(decision.Action) || r.ReasonCode != decision.ReasonCode || r.NextAttempt != decision.NextAttempt {
		return ErrInvalidRecovery
	}

	switch decision.Action {
	case knowledgefailure.ActionRetry:
		at, err := parseRecoveryTime(r.NotBefore)
		if err != nil || !at.Equal(decision.NotBefore) {
			return ErrInvalidRecovery
		}

	case knowledgefailure.ActionDeadLetter:
		if r.NotBefore != "" {
			return ErrInvalidRecovery
		}

	default:
		return ErrInvalidRecovery
	}
	return nil
}

func (r Recovery) DedupKey() string {
	if r.Kind == string(knowledgefailure.ActionRetry) {
		return "retry:" + r.SourceEventID + ":" +
			strconv.FormatUint(uint64(r.NextAttempt), 10)
	}
	if r.Kind == string(knowledgefailure.ActionDeadLetter) {
		return "dlq:" + r.SourceEventID
	}
	return ""
}

func EncodeRecovery(r Recovery) ([]byte, error) {
	if err := r.Validate(r.DocumentID); err != nil {
		return nil, err
	}
	data, err := json.Marshal(r)
	if err != nil || len(data) > MaxSourceBytes {
		return nil, ErrInvalidRecovery
	}
	return data, nil
}

func DecodeRecovery(data []byte, key string) (Recovery, error) {
	var zero Recovery
	if len(data) == 0 || len(data) > MaxSourceBytes || !utf8.Valid(data) {
		return zero, ErrInvalidRecovery
	}

	fields := map[string]bool{
		"schema_version":  false,
		"message_id":      false,
		"source_event_id": false,
		"document_id":     false,
		"source_sha256":   false,
		"generation_id":   false,
		"source_epoch":    false,
		"kind":            false,
		"failed_attempt":  false,
		"next_attempt":    false,
		"failure_code":    false,
		"reason_code":     false,
		"jitter_permille": false,
		"decided_at":      false,
		"not_before":      false,
		"trace_id":        false,
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return zero, ErrInvalidRecovery
	}

	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return zero, ErrInvalidRecovery
		}
		name, ok := token.(string)
		if !ok {
			return zero, ErrInvalidRecovery
		}
		seen, known := fields[name]
		if !known || seen {
			return zero, ErrInvalidRecovery
		}

		var value json.RawMessage
		if decoder.Decode(&value) != nil || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return zero, ErrInvalidRecovery
		}
		fields[name] = true
	}

	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return zero, ErrInvalidRecovery
	}
	for _, seen := range fields {
		if !seen {
			return zero, ErrInvalidRecovery
		}
	}
	if _, err := decoder.Token(); err != io.EOF {
		return zero, ErrInvalidRecovery
	}

	var result Recovery
	if json.Unmarshal(data, &result) != nil {
		return zero, ErrInvalidRecovery
	}
	if err := result.Validate(key); err != nil {
		return zero, err
	}
	return result, nil
}
