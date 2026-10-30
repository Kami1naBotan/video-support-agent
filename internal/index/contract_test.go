package indexing

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"video-support-agent/internal/model"
)

type contractFixture struct {
	at       time.Time
	document model.KnowledgeDocument
	version  model.RuleVersion
	article  model.RuleArticle
	chunk    model.KnowledgeChunk
}

func newContractFixture() contractFixture {
	at := time.Date(2026, 10, 29, 12, 0, 0, 123456789,
		time.FixedZone("UTC+8", 8*60*60))
	effective := at.Add(-time.Hour)
	expires := at.Add(time.Hour)
	versionID := uint64(22)

	return contractFixture{
		at: at,
		document: model.KnowledgeDocument{
			ID:                  11,
			CurrentVersionID:    &versionID,
			PublicationRevision: 0,
		},
		version: model.RuleVersion{
			ID:          versionID,
			DocumentID:  11,
			CategoryID:  55,
			Version:     "测试修订",
			Title:       "隔离测试规则",
			SourceURL:   "https://example.invalid/test-rule",
			Status:      model.RuleVersionStatusPublished,
			EffectiveAt: &effective,
			ExpiresAt:   &expires,
		},
		article: model.RuleArticle{
			ID:            33,
			VersionID:     versionID,
			ArticleOrder:  1,
			SectionPath:   "测试章节",
			ArticleNumber: "第一条",
			Content:       "条款完整正文，不应代替片段正文",
		},
		chunk: model.KnowledgeChunk{
			ID:         44,
			ArticleID:  33,
			ChunkIndex: 0,
			Content:    "测试片段\n包含中文与\"引号\"",
		},
	}
}

func (f contractFixture) convert() (Document, error) {
	return NewDocument(
		f.document, f.version, f.article, f.chunk, f.at,
	)
}

func contractSnapshot(t *testing.T) *Snapshot {
	t.Helper()

	f := newContractFixture()
	document, err := f.convert()
	if err != nil {
		t.Fatal(err)
	}

	return &Snapshot{
		SourceDatabase:   "isolated_rules",
		SourceServerUUID: "00000000-0000-0000-0000-000000000001",
		CapturedAt:       f.at,
		Documents:        []Document{document},
	}
}

func TestIndexContractDocumentProjection(t *testing.T) {
	f := newContractFixture()
	beforeEffective := *f.version.EffectiveAt
	beforeExpires := *f.version.ExpiresAt

	got, err := f.convert()
	if err != nil {
		t.Fatal(err)
	}

	if got.ChunkID != "44" ||
		got.DocumentID != "11" ||
		got.VersionID != "22" ||
		got.ArticleID != "33" ||
		got.CategoryID != "55" ||
		got.PublicationRevision != "0" {
		t.Fatalf("incorrect identity projection: %+v", got)
	}
	if got.Content != f.chunk.Content ||
		got.Title != f.version.Title ||
		got.Version != f.version.Version ||
		got.SourceURL != f.version.SourceURL ||
		got.SectionPath != f.article.SectionPath ||
		got.ArticleNumber != f.article.ArticleNumber {
		t.Fatal("source fields were lost or projected from the wrong model")
	}
	if !got.IsCurrent ||
		got.Status != string(model.RuleVersionStatusPublished) ||
		got.ChunkIndex != 0 ||
		got.ArticleOrder != 1 ||
		got.WithdrawnAt != nil {
		t.Fatal("incorrect state or ordering projection")
	}

	wantEffective := beforeEffective.UTC().Truncate(time.Millisecond)
	wantExpires := beforeExpires.UTC().Truncate(time.Millisecond)
	if got.EffectiveAt != wantEffective ||
		got.ExpiresAt == nil ||
		*got.ExpiresAt != wantExpires {
		t.Fatal("times were not normalized to UTC milliseconds")
	}
	if got.ExpiresAt == f.version.ExpiresAt {
		t.Fatal("output expiry aliases the source pointer")
	}
	if *f.version.EffectiveAt != beforeEffective ||
		*f.version.ExpiresAt != beforeExpires {
		t.Fatal("conversion mutated source timestamps")
	}

	*got.ExpiresAt = got.ExpiresAt.Add(time.Hour)
	if *f.version.ExpiresAt != beforeExpires {
		t.Fatal("mutating output expiry changed source")
	}

	f.version.ExpiresAt = nil
	f.article.SectionPath = ""
	f.article.ArticleNumber = ""
	got, err = f.convert()
	if err != nil {
		t.Fatal(err)
	}
	if got.SectionPath != "" || got.ArticleNumber != "" {
		t.Fatal("optional numbering or section was invented")
	}

	body, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(body, &object); err != nil {
		t.Fatal(err)
	}

	keys := []string{
		"chunk_id", "document_id", "version_id", "article_id",
		"category_id", "publication_revision", "chunk_index",
		"article_order", "version", "title", "section_path",
		"article_number", "content", "source_url", "status",
		"is_current", "effective_at", "expires_at", "withdrawn_at",
	}
	if len(object) != len(keys) {
		t.Fatalf("expected %d fields, got %d", len(keys), len(object))
	}
	for _, key := range keys {
		if _, exists := object[key]; !exists {
			t.Fatalf("missing JSON field %s", key)
		}
	}
	for _, key := range []string{"expires_at", "withdrawn_at"} {
		if string(object[key]) != "null" {
			t.Fatalf("%s must encode as null", key)
		}
	}
}

func TestIndexContractDocumentRejectsInvalidSource(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*contractFixture)
	}{
		{"zero_time", func(f *contractFixture) { f.at = time.Time{} }},
		{"zero_document", func(f *contractFixture) { f.document.ID = 0 }},
		{"zero_version", func(f *contractFixture) { f.version.ID = 0 }},
		{"zero_article", func(f *contractFixture) { f.article.ID = 0 }},
		{"zero_chunk", func(f *contractFixture) { f.chunk.ID = 0 }},
		{"zero_category", func(f *contractFixture) { f.version.CategoryID = 0 }},
		{"foreign_version", func(f *contractFixture) { f.version.DocumentID++ }},
		{"foreign_article", func(f *contractFixture) { f.article.VersionID++ }},
		{"foreign_chunk", func(f *contractFixture) { f.chunk.ArticleID++ }},
		{"no_current", func(f *contractFixture) {
			f.document.CurrentVersionID = nil
		}},
		{"different_current", func(f *contractFixture) {
			*f.document.CurrentVersionID = 999
		}},
		{"draft", func(f *contractFixture) {
			f.version.Status = model.RuleVersionStatusDraft
		}},
		{"withdrawn", func(f *contractFixture) {
			value := f.at
			f.version.WithdrawnAt = &value
		}},
		{"missing_effective", func(f *contractFixture) {
			f.version.EffectiveAt = nil
		}},
		{"zero_effective", func(f *contractFixture) {
			value := time.Time{}
			f.version.EffectiveAt = &value
		}},
		{"future", func(f *contractFixture) {
			value := f.at.Add(time.Millisecond)
			f.version.EffectiveAt = &value
		}},
		{"expiry_equal_now", func(f *contractFixture) {
			value := f.at
			f.version.ExpiresAt = &value
		}},
		{"expiry_before_now", func(f *contractFixture) {
			value := f.at.Add(-time.Millisecond)
			f.version.ExpiresAt = &value
		}},
		{"expiry_equal_effective", func(f *contractFixture) {
			value := *f.version.EffectiveAt
			f.version.ExpiresAt = &value
		}},
		{"zero_order", func(f *contractFixture) { f.article.ArticleOrder = 0 }},
		{"blank_version", func(f *contractFixture) { f.version.Version = " \t" }},
		{"blank_title", func(f *contractFixture) { f.version.Title = " \n" }},
		{"blank_content", func(f *contractFixture) { f.chunk.Content = "\t" }},
		{"blank_source", func(f *contractFixture) { f.version.SourceURL = " " }},
		{"invalid_utf8", func(f *contractFixture) {
			f.article.SectionPath = string([]byte{0xff})
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newContractFixture()
			tc.mutate(&f)

			got, err := f.convert()
			if !errors.Is(err, ErrInvalidSource) {
				t.Fatalf("expected ErrInvalidSource, got %v", err)
			}
			if !reflect.DeepEqual(got, Document{}) {
				t.Fatal("invalid conversion returned partial output")
			}
		})
	}
}

func TestIndexContractDocumentBoundaryValues(t *testing.T) {
	f := newContractFixture()
	effective := f.at
	f.version.EffectiveAt = &effective
	f.version.ExpiresAt = nil
	f.chunk.ID = ^uint64(0)
	f.document.PublicationRevision = ^uint64(0)
	f.chunk.ChunkIndex = ^uint32(0)
	f.article.ArticleOrder = ^uint32(0)

	got, err := f.convert()
	if err != nil {
		t.Fatal(err)
	}
	if got.ChunkID != "18446744073709551615" ||
		got.PublicationRevision != "18446744073709551615" ||
		got.ChunkIndex != ^uint32(0) ||
		got.ArticleOrder != ^uint32(0) {
		t.Fatal("large unsigned values were changed")
	}

	// Revalidate through the snapshot path as well.
	snapshot := contractSnapshot(t)
	snapshot.Documents = []Document{got}
	if _, err := PrepareSnapshot(context.Background(), snapshot); err != nil {
		t.Fatalf("valid boundary document rejected: %v", err)
	}
}

func TestIndexContractSnapshotEncoding(t *testing.T) {
	snapshot := contractSnapshot(t)
	before, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}

	got, err := PrepareSnapshot(context.Background(), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if got.SourceDatabase != snapshot.SourceDatabase ||
		got.SourceServerUUID != snapshot.SourceServerUUID ||
		got.CapturedAt != snapshot.CapturedAt.UTC().Truncate(time.Millisecond) {
		t.Fatal("snapshot identity or timestamp changed incorrectly")
	}
	if len(got.Documents) != 1 ||
		got.Documents[0].ID != snapshot.Documents[0].ChunkID {
		t.Fatal("encoded document identity mismatch")
	}
	if got.TotalJSONBytes != int64(len(got.Documents[0].JSON)) {
		t.Fatal("JSON byte accounting mismatch")
	}

	var decoded Document
	if err := json.Unmarshal(got.Documents[0].JSON, &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, snapshot.Documents[0]) {
		t.Fatal("JSON round trip changed document fields")
	}

	after, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("preparation mutated its input")
	}

	snapshot.Documents[0].Content = "changed after preparation"
	if strings.Contains(string(got.Documents[0].JSON), "changed after preparation") {
		t.Fatal("encoded output changed with source")
	}

	snapshot.Documents = []Document{}
	empty, err := PrepareSnapshot(context.Background(), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if empty.Documents == nil ||
		len(empty.Documents) != 0 ||
		empty.TotalJSONBytes != 0 {
		t.Fatal("empty snapshot must remain a non-nil empty result")
	}
}

func TestIndexContractSnapshotRejectsInvalidInput(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Snapshot)
	}{
		{"empty_database", func(s *Snapshot) { s.SourceDatabase = "" }},
		{"database_whitespace", func(s *Snapshot) { s.SourceDatabase += " " }},
		{"empty_uuid", func(s *Snapshot) { s.SourceServerUUID = "" }},
		{"invalid_identity_utf8", func(s *Snapshot) {
			s.SourceServerUUID = string([]byte{0xff})
		}},
		{"zero_capture_time", func(s *Snapshot) { s.CapturedAt = time.Time{} }},
		{"nil_documents", func(s *Snapshot) { s.Documents = nil }},
		{"duplicate", func(s *Snapshot) {
			s.Documents = append(s.Documents, s.Documents[0])
		}},
		{"not_current", func(s *Snapshot) { s.Documents[0].IsCurrent = false }},
		{"leading_zero", func(s *Snapshot) { s.Documents[0].ChunkID = "044" }},
		{"signed_id", func(s *Snapshot) { s.Documents[0].DocumentID = "+11" }},
		{"overflow_id", func(s *Snapshot) {
			s.Documents[0].VersionID = "18446744073709551616"
		}},
		{"zero_article_id", func(s *Snapshot) { s.Documents[0].ArticleID = "0" }},
		{"invalid_category", func(s *Snapshot) { s.Documents[0].CategoryID = "x" }},
		{"negative_revision", func(s *Snapshot) {
			s.Documents[0].PublicationRevision = "-1"
		}},
		{"invalid_later_document", func(s *Snapshot) {
			other := s.Documents[0]
			other.ChunkID = "45"
			other.Title = ""
			s.Documents = append(s.Documents, other)
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := contractSnapshot(t)
			tc.mutate(snapshot)

			got, err := PrepareSnapshot(context.Background(), snapshot)
			if !errors.Is(err, ErrInvalidSource) || got != nil {
				t.Fatalf("expected nil result and ErrInvalidSource, got %v, %v",
					got, err)
			}
		})
	}

	got, err := PrepareSnapshot(context.Background(), nil)
	if got != nil || !errors.Is(err, ErrInvalidSource) {
		t.Fatalf("nil snapshot: result=%v error=%v", got, err)
	}
}

// Build a valid document with an exact serialized JSON size.
// ASCII content contributes exactly one JSON byte per character.
func contractDocumentWithSize(
	t *testing.T,
	id string,
	size int,
) Document {
	t.Helper()

	document := contractSnapshot(t).Documents[0]
	document.ChunkID = id
	document.Content = ""

	base, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if size <= len(base) {
		t.Fatal("requested test size is too small")
	}

	document.Content = strings.Repeat("x", size-len(base))
	body, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) != size {
		t.Fatalf("fixture size: want %d, got %d", size, len(body))
	}

	return document
}

func TestIndexContractSnapshotLimits(t *testing.T) {
	// Do not run these large-budget subtests in parallel.
	t.Run("document_count_boundary", func(t *testing.T) {
		snapshot := contractSnapshot(t)
		template := snapshot.Documents[0]
		snapshot.Documents = make([]Document, MaxExportDocuments)

		for i := range snapshot.Documents {
			snapshot.Documents[i] = template
			snapshot.Documents[i].ChunkID = strconv.Itoa(i + 1)
		}

		got, err := PrepareSnapshot(context.Background(), snapshot)
		if err != nil || len(got.Documents) != MaxExportDocuments {
			t.Fatalf("maximum document count rejected: %v", err)
		}

		snapshot.Documents = append(snapshot.Documents, template)
		got, err = PrepareSnapshot(context.Background(), snapshot)
		if got != nil || !errors.Is(err, ErrExportLimitExceeded) {
			t.Fatalf("count overflow not rejected: %v", err)
		}
	})

	t.Run("single_document_exact_bytes", func(t *testing.T) {
		snapshot := contractSnapshot(t)
		snapshot.Documents[0] = contractDocumentWithSize(
			t, "44", MaxIndexDocumentJSONBytes,
		)

		got, err := PrepareSnapshot(context.Background(), snapshot)
		if err != nil {
			t.Fatal(err)
		}
		if got.TotalJSONBytes != int64(MaxIndexDocumentJSONBytes) {
			t.Fatal("exact JSON limit was not preserved")
		}

		snapshot.Documents[0].Content += "x"
		got, err = PrepareSnapshot(context.Background(), snapshot)
		if got != nil || !errors.Is(err, ErrExportLimitExceeded) {
			t.Fatalf("single-byte overflow not rejected: %v", err)
		}
	})

	t.Run("json_escaping_expands_content", func(t *testing.T) {
		snapshot := contractSnapshot(t)
		snapshot.Documents[0].Content =
			strings.Repeat("\"", MaxIndexDocumentJSONBytes/2+1)

		got, err := PrepareSnapshot(context.Background(), snapshot)
		if got != nil || !errors.Is(err, ErrExportLimitExceeded) {
			t.Fatalf("escaped JSON overflow not rejected: %v", err)
		}
	})

	t.Run("raw_text_limit", func(t *testing.T) {
		snapshot := contractSnapshot(t)
		snapshot.Documents[0].Content =
			strings.Repeat("x", MaxIndexDocumentJSONBytes+1)

		got, err := PrepareSnapshot(context.Background(), snapshot)
		if got != nil || !errors.Is(err, ErrExportLimitExceeded) {
			t.Fatalf("raw text overflow not rejected: %v", err)
		}
	})

	t.Run("total_json_exact_and_overflow", func(t *testing.T) {
		snapshot := contractSnapshot(t)
		snapshot.Documents = make([]Document, 0, 65)

		for i := 1; i <= 64; i++ {
			snapshot.Documents = append(
				snapshot.Documents,
				contractDocumentWithSize(
					t, strconv.Itoa(i), MaxIndexDocumentJSONBytes,
				),
			)
		}

		got, err := PrepareSnapshot(context.Background(), snapshot)
		if err != nil {
			t.Fatal(err)
		}
		if got.TotalJSONBytes != int64(MaxExportJSONBytes) {
			t.Fatalf("expected exact total limit, got %d", got.TotalJSONBytes)
		}

		// Release the reference before the next large encoding pass.
		got = nil
		extra := contractSnapshot(t).Documents[0]
		extra.ChunkID = "65"
		snapshot.Documents = append(snapshot.Documents, extra)

		got, err = PrepareSnapshot(context.Background(), snapshot)
		if got != nil || !errors.Is(err, ErrExportLimitExceeded) {
			t.Fatalf("total JSON overflow not rejected: %v", err)
		}
	})
}

type contractSourceFunc func(context.Context) (*Snapshot, error)

func (f contractSourceFunc) ExportCurrent(ctx context.Context) (*Snapshot, error) {
	return f(ctx)
}

func TestIndexContractExportAndCancellation(t *testing.T) {
	ctx := context.Background()
	snapshot := contractSnapshot(t)
	sourceFailure := errors.New("isolated source failure")

	cases := []struct {
		name   string
		source Source
		want   error
	}{
		{"nil_source", nil, ErrInvalidSource},
		{"nil_snapshot", contractSourceFunc(func(context.Context) (*Snapshot, error) {
			return nil, nil
		}), ErrInvalidSource},
		{"source_failure", contractSourceFunc(func(context.Context) (*Snapshot, error) {
			return nil, sourceFailure
		}), sourceFailure},
		{"partial_snapshot_with_error", contractSourceFunc(func(context.Context) (*Snapshot, error) {
			return snapshot, sourceFailure
		}), sourceFailure},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ExportPrepared(ctx, tc.source)
			if got != nil || !errors.Is(err, tc.want) {
				t.Fatalf("expected nil and %v, got %v, %v", tc.want, got, err)
			}
		})
	}

	t.Run("success_and_context_forwarding", func(t *testing.T) {
		type contextKey struct{}
		sourceCtx := context.WithValue(ctx, contextKey{}, "marker")
		calls := 0

		source := contractSourceFunc(func(received context.Context) (*Snapshot, error) {
			calls++
			if received.Value(contextKey{}) != "marker" {
				t.Fatal("context was not forwarded")
			}
			return snapshot, nil
		})

		got, err := ExportPrepared(sourceCtx, source)
		if err != nil || got == nil || calls != 1 {
			t.Fatalf("export failed: calls=%d error=%v", calls, err)
		}
	})

	t.Run("already_canceled", func(t *testing.T) {
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		calls := 0
		source := contractSourceFunc(func(context.Context) (*Snapshot, error) {
			calls++
			return snapshot, nil
		})

		got, err := ExportPrepared(canceled, source)
		if got != nil || !errors.Is(err, context.Canceled) || calls != 0 {
			t.Fatal("canceled export invoked source or lost cancellation")
		}

		got, err = PrepareSnapshot(canceled, snapshot)
		if got != nil || !errors.Is(err, context.Canceled) {
			t.Fatal("snapshot preparation lost cancellation")
		}
	})

	t.Run("canceled_after_export", func(t *testing.T) {
		canceled, cancel := context.WithCancel(ctx)
		defer cancel()

		source := contractSourceFunc(func(context.Context) (*Snapshot, error) {
			cancel()
			return snapshot, nil
		})

		got, err := ExportPrepared(canceled, source)
		if got != nil || !errors.Is(err, context.Canceled) {
			t.Fatal("cancellation after export was ignored")
		}
	})

	t.Run("deadline_exceeded", func(t *testing.T) {
		expired, cancel := context.WithDeadline(ctx, time.Now().Add(-time.Second))
		defer cancel()

		got, err := PrepareSnapshot(expired, snapshot)
		if got != nil || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("deadline error was not preserved")
		}
	})
}
