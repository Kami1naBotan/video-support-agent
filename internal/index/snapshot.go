package indexing

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"video-support-agent/internal/model"
)

type EncodedDocument struct {
	ID   string
	JSON json.RawMessage
}

type PreparedSnapshot struct {
	SourceDatabase   string
	SourceServerUUID string
	CapturedAt       time.Time
	Documents        []EncodedDocument
	TotalJSONBytes   int64
}

func ExportPrepared(ctx context.Context, source Source) (*PreparedSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if source == nil {
		return nil, fmt.Errorf("%w: source is required", ErrInvalidSource)
	}

	snapshot, err := source.ExportCurrent(ctx)
	if err != nil {
		return nil, fmt.Errorf("export indexing snapshot: %w", err)
	}

	prepared, err := PrepareSnapshot(ctx, snapshot)
	if err != nil {
		return nil, fmt.Errorf("prepare indexing snapshot: %w", err)
	}
	return prepared, nil
}

func PrepareSnapshot(ctx context.Context, snapshot *Snapshot) (*PreparedSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if snapshot == nil {
		return nil, fmt.Errorf("%w: snapshot is required", ErrInvalidSource)
	}

	identityFields := []struct {
		name  string
		value string
	}{
		{"source_database", snapshot.SourceDatabase},
		{"source_server_uuid", snapshot.SourceServerUUID},
	}

	for _, field := range identityFields {
		if !utf8.ValidString(field.value) || strings.TrimSpace(field.value) == "" || field.value != strings.TrimSpace(field.value) {
			return nil, fmt.Errorf("%w: %s must be non-empty valid UTF-8 without surrounding whitespace", ErrInvalidSource, field.name)
		}
	}

	if snapshot.CapturedAt.IsZero() {
		return nil, fmt.Errorf("%w: snapshot time is required", ErrInvalidSource)
	}
	if snapshot.Documents == nil {
		return nil, fmt.Errorf("%w: documents must be a non-nil slice", ErrInvalidSource)
	}
	if len(snapshot.Documents) > MaxExportDocuments {
		return nil, fmt.Errorf("%w: document count=%d", ErrExportLimitExceeded, len(snapshot.Documents))
	}

	prepared := &PreparedSnapshot{
		SourceDatabase:   snapshot.SourceDatabase,
		SourceServerUUID: snapshot.SourceServerUUID,
		CapturedAt:       snapshot.CapturedAt.UTC().Truncate(time.Millisecond),
		Documents:        make([]EncodedDocument, 0, len(snapshot.Documents)),
	}

	seen := make(map[string]struct{}, len(snapshot.Documents))

	for i, document := range snapshot.Documents {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		rawBytes := 0
		for _, value := range []string{
			document.Version,
			document.Title,
			document.SectionPath,
			document.ArticleNumber,
			document.Content,
			document.SourceURL,
		} {
			if len(value) > MaxIndexDocumentJSONBytes-rawBytes {
				return nil, fmt.Errorf("%w: document[%d] raw text exceeds limit", ErrExportLimitExceeded, i)
			}
			rawBytes += len(value)
		}

		normalized, err := normalizeSnapshotDocument(document, prepared.CapturedAt)
		if err != nil {
			return nil, fmt.Errorf("validate document[%d]: %w", i, err)
		}

		if _, exists := seen[normalized.ChunkID]; exists {
			return nil, fmt.Errorf("%w: duplicate chunk_id=%s", ErrInvalidSource, normalized.ChunkID)
		}
		seen[normalized.ChunkID] = struct{}{}

		body, err := json.Marshal(normalized)
		if err != nil {
			return nil, fmt.Errorf("%w: document[%d] cannot be encoded as JSON", ErrInvalidSource, i)
		}

		size := int64(len(body))
		if size > MaxIndexDocumentJSONBytes {
			return nil, fmt.Errorf("%w: document[%d] JSON bytes=%d", ErrExportLimitExceeded, i, size)
		}
		if size > int64(MaxExportJSONBytes)-prepared.TotalJSONBytes {
			return nil, fmt.Errorf("%w: total JSON bytes exceed limit at document[%d]", ErrExportLimitExceeded, i)
		}

		prepared.Documents = append(prepared.Documents, EncodedDocument{
			ID:   normalized.ChunkID,
			JSON: body,
		})
		prepared.TotalJSONBytes += size
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return prepared, nil
}

func normalizeSnapshotDocument(document Document, at time.Time) (Document, error) {
	idFields := []struct {
		name      string
		value     string
		allowZero bool
	}{
		{"chunk_id", document.ChunkID, false},
		{"document_id", document.DocumentID, false},
		{"version_id", document.VersionID, false},
		{"article_id", document.ArticleID, false},
		{"category_id", document.CategoryID, false},
		{"publication_revision", document.PublicationRevision, true},
	}

	var ids [6]uint64
	for i, field := range idFields {
		if len(field.value) == 0 || len(field.value) > 20 {
			return Document{}, fmt.Errorf("%w: %s must be a canonical decimal uint64", ErrInvalidSource, field.name)
		}

		value, err := strconv.ParseUint(field.value, 10, 64)
		if err != nil || strconv.FormatUint(value, 10) != field.value || (!field.allowZero && value == 0) {
			return Document{}, fmt.Errorf("%w: %s must be a canonical decimal uint64", ErrInvalidSource, field.name)
		}
		ids[i] = value
	}

	if !document.IsCurrent {
		return Document{}, fmt.Errorf("%w: indexed document must be current", ErrInvalidSource)
	}

	versionID := ids[2]
	effectiveAt := document.EffectiveAt

	return NewDocument(
		model.KnowledgeDocument{
			ID:                  ids[1],
			CurrentVersionID:    &versionID,
			PublicationRevision: ids[5],
		},
		model.RuleVersion{
			ID:          versionID,
			DocumentID:  ids[1],
			CategoryID:  ids[4],
			Version:     document.Version,
			Title:       document.Title,
			SourceURL:   document.SourceURL,
			Status:      model.RuleVersionStatus(document.Status),
			EffectiveAt: &effectiveAt,
			ExpiresAt:   document.ExpiresAt,
			WithdrawnAt: document.WithdrawnAt,
		},
		model.RuleArticle{
			ID:            ids[3],
			VersionID:     versionID,
			ArticleOrder:  document.ArticleOrder,
			SectionPath:   document.SectionPath,
			ArticleNumber: document.ArticleNumber,
		},
		model.KnowledgeChunk{
			ID:         ids[0],
			ArticleID:  ids[3],
			ChunkIndex: document.ChunkIndex,
			Content:    document.Content,
		},
		at,
	)
}
