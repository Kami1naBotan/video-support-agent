package knowledgeevent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const MaxSourceBytes = 64 << 10

const (
	DraftCreated     = "knowledge.draft.created"
	DraftUpdated     = "knowledge.draft.updated"
	RevisionCreated  = "knowledge.revision.created"
	VersionPublished = "knowledge.version.published"
	VersionWithdrawn = "knowledge.version.withdrawn"
)

var ErrInvalidSource = errors.New("invalid knowledge source event")

var (
	eventIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	tracePattern   = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

type Source struct {
	SchemaVersion       int    `json:"schema_version"`
	EventID             string `json:"event_id"`
	EventType           string `json:"event_type"`
	DocumentID          string `json:"document_id"`
	VersionID           string `json:"version_id"`
	ContentRevision     string `json:"content_revision"`
	PublicationRevision string `json:"publication_revision"`
	OccurredAt          string `json:"occurred_at"`
	TraceID             string `json:"trace_id"`
}

func invalid(field string) error {
	return fmt.Errorf("%w: %s", ErrInvalidSource, field)
}

func canonicalUint(value string, allowZero bool) bool {
	n, err := strconv.ParseUint(value, 10, 64)
	return err == nil && (allowZero || n > 0) && strconv.FormatUint(n, 10) == value
}

func (s Source) Validate(key string) error {
	if s.SchemaVersion != 1 {
		return invalid("schema_version")
	}
	if !eventIDPattern.MatchString(s.EventID) {
		return invalid("event_id")
	}

	switch s.EventType {
	case DraftCreated, DraftUpdated, RevisionCreated,
		VersionPublished, VersionWithdrawn:
	default:
		return invalid("event_type")
	}

	if !canonicalUint(s.DocumentID, false) || key != s.DocumentID {
		return invalid("document_id or key")
	}
	if !canonicalUint(s.VersionID, false) {
		return invalid("version_id")
	}
	if !canonicalUint(s.ContentRevision, false) {
		return invalid("content_revision")
	}

	allowZero := s.EventType != VersionPublished && s.EventType != VersionWithdrawn
	if !canonicalUint(s.PublicationRevision, allowZero) {
		return invalid("publication_revision")
	}
	if !tracePattern.MatchString(s.TraceID) ||
		s.TraceID == strings.Repeat("0", 32) {
		return invalid("trace_id")
	}

	at, err := time.Parse(time.RFC3339Nano, s.OccurredAt)
	if err != nil || at.IsZero() ||
		!strings.HasSuffix(s.OccurredAt, "Z") {
		return invalid("occurred_at")
	}

	return nil
}

func Encode(s Source) ([]byte, error) {
	if err := s.Validate(s.DocumentID); err != nil {
		return nil, err
	}

	data, err := json.Marshal(s)
	if err != nil || len(data) > MaxSourceBytes {
		return nil, invalid("encoding or size")
	}

	return data, nil
}

func Decode(data []byte, key string) (Source, error) {
	if len(data) == 0 || len(data) > MaxSourceBytes || !utf8.Valid(data) {
		return Source{}, invalid("size or encoding")
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return Source{}, invalid("JSON object")
	}

	seen := make(map[string]bool, 9)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return Source{}, invalid("JSON field")
		}

		name, ok := token.(string)
		if !ok || seen[name] {
			return Source{}, invalid("duplicate field")
		}

		switch name {
		case "schema_version", "event_id", "event_type",
			"document_id", "version_id", "content_revision",
			"publication_revision", "occurred_at", "trace_id":
		default:
			return Source{}, invalid("unknown field")
		}

		seen[name] = true

		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return Source{}, invalid("JSON value")
		}
	}
	if token, err := decoder.Token(); err != nil ||
		token != json.Delim('}') {
		return Source{}, invalid("JSON object end")
	}

	if len(seen) != 9 {
		return Source{}, invalid("missing field")
	}

	if _, err := decoder.Token(); err != io.EOF {
		return Source{}, invalid("trailing data")
	}

	var source Source
	if err := json.Unmarshal(data, &source); err != nil {
		return Source{}, invalid("field type")
	}
	if err := source.Validate(key); err != nil {
		return Source{}, err
	}

	return source, nil
}
