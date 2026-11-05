package knowledgeevent

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func sourceFixture() Source {
	return Source{1, "11111111-1111-4111-8111-111111111111", DraftCreated, "1", "2", "1", "0", "2026-11-05T00:00:00Z", "1234567890abcdef1234567890abcdef"}
}

func TestSourceCodecContract(t *testing.T) {
	for _, kind := range []string{DraftCreated, DraftUpdated, RevisionCreated, VersionPublished, VersionWithdrawn} {
		t.Run(kind, func(t *testing.T) {
			s := sourceFixture()
			s.EventType = kind
			s.PublicationRevision = "1"
			b, err := Encode(s)
			if err != nil {
				t.Fatal(err)
			}
			got, err := Decode(b, "1")
			if err != nil || got != s {
				t.Fatalf("roundtrip: %+v %v", got, err)
			}
		})
	}
	b, _ := Encode(sourceFixture())
	raw := string(b)
	cases := map[string]string{
		"duplicate":         strings.Replace(raw, `"schema_version":1`, `"schema_version":1,"schema_version":1`, 1),
		"escaped_duplicate": strings.Replace(raw, `"schema_version":1`, `"schema_version":1,"schema_versio\u006e":1`, 1),
		"unknown":           strings.Replace(raw, `"schema_version":1`, `"extra":1,"schema_version":1`, 1),
		"missing":           strings.Replace(raw, `"schema_version":1,`, "", 1),
		"wrong_type":        strings.Replace(raw, `"document_id":"1"`, `"document_id":1`, 1),
		"null":              strings.Replace(raw, `"trace_id":"1234567890abcdef1234567890abcdef"`, `"trace_id":null`, 1),
		"noncanonical":      strings.Replace(raw, `"document_id":"1"`, `"document_id":"01"`, 1),
		"overflow":          strings.Replace(raw, `"version_id":"2"`, `"version_id":"18446744073709551616"`, 1),
		"zero_revision":     strings.Replace(raw, `"content_revision":"1"`, `"content_revision":"0"`, 1),
		"zero_publish":      strings.Replace(raw, DraftCreated, VersionPublished, 1),
		"bad_kind":          strings.Replace(raw, DraftCreated, "unknown", 1),
		"local_time":        strings.Replace(raw, "00:00:00Z", "00:00:00+00:00", 1),
		"trailing":          raw + ` {}`,
		"array":             "[]",
		"utf8":              raw + string([]byte{255}),
		"size":              strings.Repeat(" ", MaxSourceBytes) + raw,
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode([]byte(input), "1"); !errors.Is(err, ErrInvalidSource) {
				t.Fatalf("accepted: %v", err)
			}
		})
	}
	if _, err := Decode(b, "2"); !errors.Is(err, ErrInvalidSource) {
		t.Fatal("key mismatch accepted")
	}
}

func TestSourceIdentityTrace(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id, err := NewID()
		if err != nil || !eventIDPattern.MatchString(id) || seen[id] {
			t.Fatalf("identity %q %v", id, err)
		}
		seen[id] = true
	}
	ctx, trace, err := EnsureTrace(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, again, err := EnsureTrace(ctx)
	if err != nil || again != trace {
		t.Fatal("trace changed")
	}
	for _, bad := range []string{"", strings.Repeat("0", 32), strings.Repeat("A", 32)} {
		if _, err := WithTrace(ctx, bad); err == nil {
			t.Fatal("bad trace accepted")
		}
	}
}
