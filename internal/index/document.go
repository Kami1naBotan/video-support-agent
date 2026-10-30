package indexing

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"video-support-agent/internal/model"
)

const (
	MaxExportDocuments        = 10000
	MaxIndexDocumentJSONBytes = 1 << 20
	MaxExportJSONBytes        = 64 << 20
)

var (
	ErrInvalidSource       = errors.New("invalid indexing source")
	ErrExportLimitExceeded = errors.New("index export limit exceeded")
)

type Document struct {
	ChunkID             string     `json:"chunk_id"`
	DocumentID          string     `json:"document_id"`
	VersionID           string     `json:"version_id"`
	ArticleID           string     `json:"article_id"`
	CategoryID          string     `json:"category_id"`
	PublicationRevision string     `json:"publication_revision"`
	ChunkIndex          uint32     `json:"chunk_index"`
	ArticleOrder        uint32     `json:"article_order"`
	Version             string     `json:"version"`
	Title               string     `json:"title"`
	SectionPath         string     `json:"section_path"`
	ArticleNumber       string     `json:"article_number"`
	Content             string     `json:"content"`
	SourceURL           string     `json:"source_url"`
	Status              string     `json:"status"`
	IsCurrent           bool       `json:"is_current"`
	EffectiveAt         time.Time  `json:"effective_at"`
	ExpiresAt           *time.Time `json:"expires_at"`
	WithdrawnAt         *time.Time `json:"withdrawn_at"`
}

type Snapshot struct {
	SourceDatabase   string
	SourceServerUUID string
	CapturedAt       time.Time
	Documents        []Document
}

type Source interface {
	ExportCurrent(ctx context.Context) (*Snapshot, error)
}

func NewDocument(
	document model.KnowledgeDocument,
	version model.RuleVersion,
	article model.RuleArticle,
	chunk model.KnowledgeChunk,
	at time.Time,
) (Document, error) {
	invalid := func(reason string) (Document, error) {
		return Document{}, fmt.Errorf(
			"%w: document_id=%d version_id=%d article_id=%d chunk_id=%d: %s",
			ErrInvalidSource,
			document.ID,
			version.ID,
			article.ID,
			chunk.ID,
			reason,
		)
	}

	if at.IsZero() {
		return invalid("export time is required")
	}
	if document.ID == 0 || version.ID == 0 || article.ID == 0 || chunk.ID == 0 || version.CategoryID == 0 {
		return invalid("source IDs must not be zero")
	}

	if version.DocumentID != document.ID {
		return invalid("version does not belong to document")
	}
	if article.VersionID != version.ID {
		return invalid("article does not belong to version")
	}
	if chunk.ArticleID != article.ID {
		return invalid("chunk does not belong to article")
	}
	if document.CurrentVersionID == nil ||
		*document.CurrentVersionID != version.ID {
		return invalid("version is not selected as current")
	}

	if version.Status != model.RuleVersionStatusPublished {
		return invalid("version is not published")
	}
	if version.WithdrawnAt != nil {
		return invalid("version is withdrawn")
	}
	if version.EffectiveAt == nil || version.EffectiveAt.IsZero() {
		return invalid("effective time is required")
	}
	if version.ExpiresAt != nil &&
		!version.ExpiresAt.After(*version.EffectiveAt) {
		return invalid("expiry must be after effective time")
	}

	at = at.UTC().Truncate(time.Millisecond)
	effectiveAt := version.EffectiveAt.UTC().Truncate(time.Millisecond)
	expiresAt := copyIndexTime(version.ExpiresAt)

	if effectiveAt.After(at) {
		return invalid("version is not yet effective")
	}
	if expiresAt != nil && !expiresAt.After(at) {
		return invalid("version has expired")
	}
	if article.ArticleOrder == 0 {
		return invalid("article order must be positive")
	}

	textFields := []struct {
		name     string
		value    string
		required bool
	}{
		{"version", version.Version, true},
		{"title", version.Title, true},
		{"section_path", article.SectionPath, false},
		{"article_number", article.ArticleNumber, false},
		{"content", chunk.Content, true},
		{"source_url", version.SourceURL, true},
	}

	for _, field := range textFields {
		if !utf8.ValidString(field.value) {
			return invalid(field.name + " must contain valid UTF-8")
		}
		if field.required && strings.TrimSpace(field.value) == "" {
			return invalid(field.name + " must not be blank")
		}
	}

	return Document{
		ChunkID:             strconv.FormatUint(chunk.ID, 10),
		DocumentID:          strconv.FormatUint(document.ID, 10),
		VersionID:           strconv.FormatUint(version.ID, 10),
		ArticleID:           strconv.FormatUint(article.ID, 10),
		CategoryID:          strconv.FormatUint(version.CategoryID, 10),
		PublicationRevision: strconv.FormatUint(document.PublicationRevision, 10),
		ChunkIndex:          chunk.ChunkIndex,
		ArticleOrder:        article.ArticleOrder,
		Version:             version.Version,
		Title:               version.Title,
		SectionPath:         article.SectionPath,
		ArticleNumber:       article.ArticleNumber,
		Content:             chunk.Content,
		SourceURL:           version.SourceURL,
		Status:              string(version.Status),
		IsCurrent:           true,
		EffectiveAt:         effectiveAt,
		ExpiresAt:           expiresAt,
		WithdrawnAt:         nil,
	}, nil
}

func copyIndexTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}

	copied := value.UTC().Truncate(time.Millisecond)
	return &copied
}
