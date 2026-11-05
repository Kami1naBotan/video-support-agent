package knowledgeevent

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"
	"unicode/utf8"
	"video-support-agent/internal/knowledgefailure"
)

var ErrInvalidResult = errors.New("invalid knowledge index result")

const (
	ResultApplied      = "applied"
	ResultIgnoredDraft = "ignored_draft"
	ResultSuperseded   = "superseded"
	ResultDeadLettered = "dead_lettered"
)

type Result struct {
	SchemaVersion       int     `json:"schema_version"`
	MessageID           string  `json:"message_id"`
	SourceEventID       string  `json:"source_event_id"`
	DocumentID          string  `json:"document_id"`
	VersionID           string  `json:"version_id"`
	PublicationRevision string  `json:"publication_revision"`
	Attempt             uint32  `json:"attempt"`
	Outcome             string  `json:"outcome"`
	GenerationID        *string `json:"generation_id"`
	Epoch               string  `json:"epoch"`
	OccurredAt          string  `json:"occurred_at"`
	TraceID             string  `json:"trace_id"`
}

func (r Result) Validate(key string) error {
	if r.SchemaVersion != 1 ||
		!eventIDPattern.MatchString(r.MessageID) ||
		!eventIDPattern.MatchString(r.SourceEventID) ||
		r.MessageID == r.SourceEventID ||
		!canonicalUint(r.DocumentID, false) ||
		key != r.DocumentID ||
		!canonicalUint(r.VersionID, false) ||
		r.Attempt < 1 || r.Attempt > knowledgefailure.MaxAttempts ||
		!tracePattern.MatchString(r.TraceID) ||
		r.TraceID == strings.Repeat("0", 32) {
		return ErrInvalidResult
	}

	switch r.Outcome {
	case ResultApplied:
		if !canonicalUint(r.PublicationRevision, false) ||
			r.GenerationID == nil ||
			!eventIDPattern.MatchString(*r.GenerationID) ||
			!canonicalUint(r.Epoch, false) {
			return ErrInvalidResult
		}
	case ResultIgnoredDraft, ResultSuperseded, ResultDeadLettered:
		allowZero := r.Outcome != ResultSuperseded
		if !canonicalUint(r.PublicationRevision, allowZero) || r.GenerationID != nil || r.Epoch != "0" {
			return ErrInvalidResult
		}
	default:
		return ErrInvalidResult
	}

	at, err := time.Parse(time.RFC3339Nano, r.OccurredAt)
	if err != nil || at.IsZero() || !strings.HasSuffix(r.OccurredAt, "Z") {
		return ErrInvalidResult
	}
	return nil
}

func EncodeResult(result Result) ([]byte, error) {
	if err := result.Validate(result.DocumentID); err != nil {
		return nil, err
	}
	data, err := json.Marshal(result)
	if err != nil || len(data) > MaxSourceBytes {
		return nil, ErrInvalidResult
	}
	return data, nil
}

func DecodeResult(data []byte, key string) (Result, error) {
	var zero Result
	if len(data) == 0 || len(data) > MaxSourceBytes || !utf8.Valid(data) {
		return zero, ErrInvalidResult
	}

	fields := map[string]bool{
		"schema_version":       false,
		"message_id":           false,
		"source_event_id":      false,
		"document_id":          false,
		"version_id":           false,
		"publication_revision": false,
		"attempt":              false,
		"outcome":              false,
		"generation_id":        false,
		"epoch":                false,
		"occurred_at":          false,
		"trace_id":             false,
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return zero, ErrInvalidResult
	}

	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return zero, ErrInvalidResult
		}
		name, ok := token.(string)
		if !ok {
			return zero, ErrInvalidResult
		}
		seen, known := fields[name]
		if !known || seen {
			return zero, ErrInvalidResult
		}

		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return zero, ErrInvalidResult
		}
		if name != "generation_id" &&
			bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return zero, ErrInvalidResult
		}
		fields[name] = true
	}

	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return zero, ErrInvalidResult
	}
	for _, seen := range fields {
		if !seen {
			return zero, ErrInvalidResult
		}
	}
	if _, err := decoder.Token(); err != io.EOF {
		return zero, ErrInvalidResult
	}

	var result Result
	if json.Unmarshal(data, &result) != nil {
		return zero, ErrInvalidResult
	}
	if err := result.Validate(key); err != nil {
		return zero, err
	}
	return result, nil
}
