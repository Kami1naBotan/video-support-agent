package indexing

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

type PublicationState string

const (
	PublicationNotAttempted PublicationState = "not_attempted"
	PublicationApplied      PublicationState = "applied"
	PublicationOldObserved  PublicationState = "old_observed"
	PublicationUnknown      PublicationState = "unknown"
)

type PublicationResult struct {
	State               PublicationState
	ObservedTarget      string
	RequestAcknowledged bool
}

type IndexWriter interface {
	GetAliasTarget(ctx context.Context) (string, error)
	CreateIndex(ctx context.Context, index string, definition []byte) error
	BulkIndex(ctx context.Context, index string, documents []EncodedDocument) error
	VerifyIndex(ctx context.Context, index string, documents []EncodedDocument) error
	SwitchAlias(ctx context.Context, expectedOld string, newIndex string) (PublicationResult, error)
}

type RebuildOptions struct {
	NewIndex           string
	Definition         []byte
	ExpectedDatabase   string
	ExpectedServerUUID string
	AllowEmpty         bool
}

type RebuildResult struct {
	Stage            string
	OldIndex         string
	NewIndex         string
	SourceDatabase   string
	SourceServerUUID string
	CapturedAt       time.Time
	DocumentCount    int
	TotalJSONBytes   int64
	Publication      PublicationResult
}

func Rebuild(ctx context.Context, source Source, writer IndexWriter, options RebuildOptions) (*RebuildResult, error) {
	result := &RebuildResult{
		Stage:    "preflight",
		NewIndex: options.NewIndex,
		Publication: PublicationResult{
			State: PublicationNotAttempted,
		},
	}

	if err := ctx.Err(); err != nil {
		return result, err
	}
	if source == nil || writer == nil {
		return result, fmt.Errorf("%w: source and index writer are required", ErrInvalidSource)
	}

	for _, value := range []string{options.ExpectedDatabase, options.ExpectedServerUUID} {
		if !utf8.ValidString(value) || strings.TrimSpace(value) == "" || value != strings.TrimSpace(value) {
			return result, fmt.Errorf("%w: expected source identity is required without surrounding whitespace", ErrInvalidSource)
		}
	}
	if options.NewIndex == "" || len(options.Definition) == 0 {
		return result, fmt.Errorf("%w: new index and definition are required", ErrInvalidSource)
	}

	result.Stage = "export"
	prepared, err := ExportPrepared(ctx, source)
	if err != nil {
		return result, err
	}

	result.SourceDatabase = prepared.SourceDatabase
	result.SourceServerUUID = prepared.SourceServerUUID
	result.CapturedAt = prepared.CapturedAt
	result.DocumentCount = len(prepared.Documents)
	result.TotalJSONBytes = prepared.TotalJSONBytes

	result.Stage = "source-check"
	if prepared.SourceDatabase != options.ExpectedDatabase || prepared.SourceServerUUID != options.ExpectedServerUUID {
		return result, fmt.Errorf("%w: source identity does not match operator expectation", ErrInvalidSource)
	}
	if len(prepared.Documents) == 0 && !options.AllowEmpty {
		return result, fmt.Errorf("%w: empty rebuild requires explicit authorization", ErrInvalidSource)
	}

	result.Stage = "alias-read"
	oldIndex, err := writer.GetAliasTarget(ctx)
	if err != nil {
		return result, fmt.Errorf("read current alias: %w", err)
	}
	result.OldIndex = oldIndex
	if oldIndex == options.NewIndex {
		return result, fmt.Errorf("%w: new index is already the current target", ErrInvalidSource)
	}

	result.Stage = "create"
	if err := writer.CreateIndex(ctx, options.NewIndex, options.Definition); err != nil {
		return result, fmt.Errorf("create rebuild index: %w", err)
	}

	result.Stage = "bulk"
	if err := writer.BulkIndex(ctx, options.NewIndex, prepared.Documents); err != nil {
		return result, fmt.Errorf("write rebuild index: %w", err)
	}

	result.Stage = "verify"
	if err := writer.VerifyIndex(ctx, options.NewIndex, prepared.Documents); err != nil {
		return result, fmt.Errorf("verify rebuild index: %w", err)
	}

	if err := ctx.Err(); err != nil {
		return result, err
	}

	result.Stage = "publish"
	publication, err := writer.SwitchAlias(ctx, oldIndex, options.NewIndex)
	result.Publication = publication
	if err != nil {
		return result, fmt.Errorf("publish rebuild index: %w", err)
	}
	if publication.State != PublicationApplied || publication.ObservedTarget != options.NewIndex {
		return result, fmt.Errorf("publish rebuild index: new target not confirmed")
	}

	result.Stage = "complete"
	return result, nil
}
