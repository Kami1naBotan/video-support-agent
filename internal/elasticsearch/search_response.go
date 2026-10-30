package elasticsearch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	indexing "video-support-agent/internal/index"
	"video-support-agent/internal/retrieval"
)

type searchHit struct {
	Index     string              `json:"_index"`
	ID        string              `json:"_id"`
	Score     *float64            `json:"_score"`
	Source    json.RawMessage     `json:"_source"`
	Highlight map[string][]string `json:"highlight"`
}

type searchEnvelope struct {
	TimedOut        *bool `json:"timed_out"`
	TerminatedEarly *bool `json:"terminated_early"`
	Shards          *struct {
		Total      *int `json:"total"`
		Successful *int `json:"successful"`
		Failed     *int `json:"failed"`
	} `json:"_shards"`
	Hits *struct {
		Items *[]searchHit `json:"hits"`
	} `json:"hits"`
}

func decodeSearchCandidates(ctx context.Context, data []byte, target string, opts retrieval.SearchOptions, at time.Time) ([]retrieval.Candidate, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(data) > MaxResponseBytes || !utf8.Valid(data) {
		return nil, invalidSearchResponse("invalid response encoding or size")
	}

	var envelope searchEnvelope
	if json.Unmarshal(data, &envelope) != nil {
		return nil, invalidSearchResponse("malformed search JSON")
	}

	if envelope.TimedOut == nil || envelope.Shards == nil || envelope.Shards.Total == nil || envelope.Shards.Successful == nil || envelope.Shards.Failed == nil || envelope.Hits == nil || envelope.Hits.Items == nil {
		return nil, invalidSearchResponse("required search fields are missing")
	}

	if *envelope.TimedOut {
		return nil, fmt.Errorf("%w: search timed out: %w", ErrUnavailable, context.DeadlineExceeded)
	}
	if envelope.TerminatedEarly != nil && *envelope.TerminatedEarly {
		return nil, invalidSearchResponse("search terminated early")
	}

	shards := envelope.Shards
	if *shards.Total <= 0 || *shards.Successful != *shards.Total || *shards.Failed != 0 {
		return nil, invalidSearchResponse("incomplete search shard results")
	}

	hits := *envelope.Hits.Items
	limit := opts.Limit * retrieval.CandidateMultiplier
	if limit < 1 || limit > retrieval.MaxCandidates || len(hits) > limit {
		return nil, invalidSearchResponse("candidate count exceeds the request")
	}

	result := make([]retrieval.Candidate, 0, len(hits))
	seen := make(map[string]struct{}, len(hits))
	var previousID string
	var previousScore float64

	for i, hit := range hits {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		if hit.Index != target || hit.Score == nil {
			return nil, invalidSearchResponse("hit index or score is invalid")
		}
		score := *hit.Score
		if math.IsNaN(score) || math.IsInf(score, 0) || score < 0 {
			return nil, invalidSearchResponse("hit score is invalid")
		}
		if _, exists := seen[hit.ID]; exists {
			return nil, invalidSearchResponse("duplicate hit ID")
		}
		if i > 0 && (score > previousScore || (score == previousScore && hit.ID <= previousID)) {
			return nil, invalidSearchResponse("hit order differs from requested sort")
		}

		candidate, err := decodeSearchSource(hit, opts, at)
		if err != nil {
			return nil, err
		}

		result = append(result, candidate)
		seen[hit.ID] = struct{}{}
		previousID = hit.ID
		previousScore = score
	}

	return result, nil
}

func decodeSearchSource(hit searchHit, opts retrieval.SearchOptions, at time.Time) (retrieval.Candidate, error) {
	fail := func(reason string) (retrieval.Candidate, error) {
		return retrieval.Candidate{}, invalidSearchResponse(reason)
	}

	var fields map[string]json.RawMessage
	if json.Unmarshal(hit.Source, &fields) != nil || fields == nil {
		return fail("hit source must be an object")
	}

	required := []string{
		"chunk_id", "document_id", "version_id", "article_id",
		"category_id", "publication_revision",
		"chunk_index", "article_order",
		"version", "title", "section_path", "article_number",
		"content", "source_url", "status", "is_current",
		"effective_at", "expires_at", "withdrawn_at",
	}
	for _, name := range required {
		value, exists := fields[name]
		if !exists {
			return fail("required source field is missing")
		}
		if name != "expires_at" && name != "withdrawn_at" &&
			bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return fail("required source field is null")
		}
	}

	var source indexing.Document
	if json.Unmarshal(hit.Source, &source) != nil {
		return fail("source field types are invalid")
	}
	if hit.ID != source.ChunkID {
		return fail("hit ID differs from source chunk ID")
	}

	values := []string{
		source.ChunkID,
		source.DocumentID,
		source.VersionID,
		source.ArticleID,
		source.CategoryID,
		source.PublicationRevision,
	}
	ids := make([]uint64, len(values))
	for i, value := range values {
		id, err := strconv.ParseUint(value, 10, 64)
		if err != nil || strconv.FormatUint(id, 10) != value || (i < 5 && id == 0) {
			return fail("source ID is not a canonical decimal value")
		}
		ids[i] = id
	}

	for _, value := range []string{source.Version, source.Title, source.Content, source.SourceURL} {
		if strings.TrimSpace(value) == "" {
			return fail("required source text is blank")
		}
	}

	if source.ArticleOrder == 0 || !source.IsCurrent || source.Status != "published" || source.WithdrawnAt != nil {
		return fail("source state contradicts the search filters")
	}
	if source.EffectiveAt.IsZero() || source.EffectiveAt.After(at) {
		return fail("source effective time contradicts the search filters")
	}
	if source.ExpiresAt != nil && (!source.ExpiresAt.After(at) || !source.ExpiresAt.After(source.EffectiveAt)) {
		return fail("source expiry contradicts the search filters")
	}

	if (opts.DocumentID != 0 && opts.DocumentID != ids[1]) || (opts.CategoryID != 0 && opts.CategoryID != ids[4]) || (opts.ArticleNumber != "" && opts.ArticleNumber != source.ArticleNumber) {
		return fail("source identity contradicts the requested filters")
	}

	highlights, err := decodeSearchHighlights(hit.Highlight, opts.Highlight)
	if err != nil {
		return retrieval.Candidate{}, err
	}

	var expiresAt *time.Time
	if source.ExpiresAt != nil {
		value := source.ExpiresAt.UTC()
		expiresAt = &value
	}

	return retrieval.Candidate{
		PublicationRevision: ids[5],
		Chunk: retrieval.RuleChunk{
			ChunkID:       ids[0],
			DocumentID:    ids[1],
			VersionID:     ids[2],
			ArticleID:     ids[3],
			CategoryID:    ids[4],
			ChunkIndex:    source.ChunkIndex,
			ArticleOrder:  source.ArticleOrder,
			Version:       source.Version,
			Title:         source.Title,
			SectionPath:   source.SectionPath,
			ArticleNumber: source.ArticleNumber,
			Content:       source.Content,
			SourceURL:     source.SourceURL,
			EffectiveAt:   source.EffectiveAt.UTC(),
			ExpiresAt:     expiresAt,
			Score:         *hit.Score,
			Highlights:    highlights,
		},
	}, nil
}

func decodeSearchHighlights(input map[string][]string, enabled bool) (map[string][]string, error) {
	result := make(map[string][]string)

	if !enabled && len(input) != 0 {
		return nil, invalidSearchResponse("unexpected highlights")
	}

	for field, fragments := range input {
		maxFragments := 0
		switch field {
		case "title":
			maxFragments = 1
		case "section_path":
			maxFragments = 2
		case "content":
			maxFragments = 3
		default:
			return nil, invalidSearchResponse("unsupported highlight field")
		}

		if len(fragments) == 0 || len(fragments) > maxFragments {
			return nil, invalidSearchResponse("invalid highlight fragment count")
		}

		copied := make([]string, len(fragments))
		for i, fragment := range fragments {
			if fragment == "" || !utf8.ValidString(fragment) {
				return nil, invalidSearchResponse("invalid highlight fragment")
			}

			text := strings.ReplaceAll(fragment, "<em>", "")
			text = strings.ReplaceAll(text, "</em>", "")
			if strings.ContainsAny(text, "<>") {
				return nil, invalidSearchResponse("unexpected raw highlight markup")
			}
			copied[i] = fragment
		}
		result[field] = copied
	}

	return result, nil
}

func invalidSearchResponse(reason string) error {
	return fmt.Errorf("%w: %s", ErrResponse, reason)
}
