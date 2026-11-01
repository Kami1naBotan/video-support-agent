package milvus

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

	"video-support-agent/internal/embedding"
	"video-support-agent/internal/retrieval"
)

type SearchHit struct {
	ChunkID             uint64
	DocumentID          uint64
	VersionID           uint64
	ArticleID           uint64
	CategoryID          uint64
	PublicationRevision uint64
	Version             string
	EffectiveAt         time.Time
	ExpiresAt           *time.Time
	Score               float64
}

func (c *Client) SearchPrepared(ctx context.Context, query *PreparedQuery) ([]SearchHit, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, ok := ctx.Deadline(); !ok {
		return nil, fmt.Errorf("%w: search requires an overall deadline", ErrInput)
	}
	if c == nil || c.http == nil {
		return nil, fmt.Errorf("%w: search client is not configured", ErrInput)
	}

	q, err := copySearchQuery(query)
	if err != nil {
		return nil, err
	}

	target, err := c.GetAliasTarget(ctx)
	if err != nil {
		return nil, err
	}
	if target == "" {
		return nil, fmt.Errorf("%w: vector alias is absent", retrieval.ErrIndexNotReady)
	}

	collectionID, err := c.VerifyCollectionContract(ctx, target, q.Identity)
	if err != nil {
		return nil, err
	}
	if err := c.checkSearchReady(ctx, target); err != nil {
		return nil, err
	}

	outputFields := make([]string, 0, 10)
	for _, field := range collectionFields() {
		if field.Name != "embedding" {
			outputFields = append(outputFields, field.Name)
		}
	}

	data, err := c.request(ctx, "entities/search", map[string]any{
		"collectionName":   target,
		"annsField":        "embedding",
		"data":             [][]float32{q.Vector},
		"filter":           searchFilter(q),
		"limit":            q.CandidateLimit,
		"outputFields":     outputFields,
		"consistencyLevel": "Strong",
		"searchParams": map[string]any{
			"metricType": "COSINE",
			"params": map[string]any{
				"ef": max(64, q.CandidateLimit),
			},
		},
	}, false)
	if err != nil {
		return nil, err
	}

	var rows []json.RawMessage
	if json.Unmarshal(data, &rows) != nil ||
		rows == nil || len(rows) > q.CandidateLimit {
		return nil, fmt.Errorf("%w: invalid search result list", ErrResponse)
	}

	hits := make([]SearchHit, 0, len(rows))
	seen := make(map[uint64]struct{}, len(rows))
	previousScore := math.Inf(1)

	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		hit, err := decodeSearchHit(row, q)
		if err != nil {
			return nil, err
		}
		if _, exists := seen[hit.ChunkID]; exists {
			return nil, fmt.Errorf("%w: duplicate search ID", ErrResponse)
		}
		seen[hit.ChunkID] = struct{}{}

		if hit.Score > previousScore+1e-6 {
			return nil, fmt.Errorf("%w: search scores are out of order", ErrResponse)
		}
		previousScore = hit.Score
		hits = append(hits, hit)
	}

	afterID, err := c.VerifyCollectionContract(ctx, target, q.Identity)
	if err != nil {
		return nil, err
	}
	if afterID != collectionID {
		return nil, fmt.Errorf("%w: search collection identity changed", ErrResponse)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return hits, nil
}

func copySearchQuery(query *PreparedQuery) (*PreparedQuery, error) {
	if query == nil {
		return nil, fmt.Errorf("%w: prepared query is required", ErrInput)
	}
	q := *query
	opts := q.Options

	if opts.Limit < 1 || opts.Limit > retrieval.MaxLimit || opts.Section != "" || opts.ArticleNumber != "" || opts.Highlight || q.CandidateLimit != opts.Limit*retrieval.CandidateMultiplier || q.CandidateLimit > retrieval.MaxCandidates {
		return nil, fmt.Errorf("%w: invalid prepared search options", ErrInput)
	}
	if q.At.IsZero() || q.At.Year() < 1 || q.At.Year() > 9999 {
		return nil, fmt.Errorf("%w: invalid prepared search time", ErrInput)
	}
	q.At = q.At.UTC().Truncate(time.Millisecond)

	if err := checkIdentity(q.Identity, q.Identity.Digest); err != nil {
		return nil, err
	}
	if len(q.Vector) != embedding.Dimensions {
		return nil, fmt.Errorf("%w: invalid search vector dimensions", ErrInput)
	}
	raw := make([]float64, len(q.Vector))
	for i, value := range q.Vector {
		raw[i] = float64(value)
	}
	vectors, err := embedding.ValidateVectors([][]float64{raw}, 1)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid search vector: %w", ErrInput, err)
	}
	q.Vector = vectors[0]
	return &q, nil
}

func (c *Client) checkSearchReady(ctx context.Context, target string) error {
	ready, err := c.indexReady(ctx, target)
	if err != nil {
		return err
	}
	if !ready {
		return fmt.Errorf("%w: vector index is not ready", retrieval.ErrIndexNotReady)
	}

	data, err := c.request(ctx, "collections/get_load_state", map[string]any{"collectionName": target}, false)
	if err != nil {
		return err
	}
	var state struct {
		LoadState *string `json:"loadState"`
	}
	if json.Unmarshal(data, &state) != nil || state.LoadState == nil {
		return fmt.Errorf("%w: invalid search load state", ErrResponse)
	}
	switch *state.LoadState {
	case "LoadStateLoaded":
		return nil
	case "LoadStateLoading", "LoadStateNotLoad", "LoadStateNotExist":
		return fmt.Errorf("%w: vector collection is not loaded", retrieval.ErrIndexNotReady)
	default:
		return fmt.Errorf("%w: unexpected search load state", ErrResponse)
	}
}

func searchFilter(q *PreparedQuery) string {
	at := strconv.FormatInt(q.At.UnixMilli(), 10)
	conditions := []string{
		"effective_at <= " + at,
		"(has_expires_at == false or expires_at > " + at + ")",
	}
	if q.Options.DocumentID != 0 {
		conditions = append(conditions, `document_id == "`+strconv.FormatUint(q.Options.DocumentID, 10)+`"`)
	}
	if q.Options.CategoryID != 0 {
		conditions = append(conditions, `category == "`+strconv.FormatUint(q.Options.CategoryID, 10)+`"`)
	}
	return strings.Join(conditions, " and ")
}

func decodeSearchHit(data []byte, q *PreparedQuery) (SearchHit, error) {
	var zero SearchHit
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || len(fields) != 11 {
		return zero, fmt.Errorf("%w: invalid search hit fields", ErrResponse)
	}
	for _, name := range []string{
		"chunk_id", "document_id", "version_id", "article_id",
		"category", "publication_revision", "version",
		"effective_at", "has_expires_at", "expires_at", "distance",
	} {
		value, exists := fields[name]
		if !exists || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return zero, fmt.Errorf("%w: missing or null search field", ErrResponse)
		}
	}

	var wire struct {
		Record
		Distance float64 `json:"distance"`
	}
	if json.Unmarshal(data, &wire) != nil {
		return zero, fmt.Errorf("%w: invalid search field types", ErrResponse)
	}

	rawIDs := []string{
		wire.ChunkID, wire.DocumentID, wire.VersionID,
		wire.ArticleID, wire.Category, wire.PublicationRevision,
	}
	ids := make([]uint64, len(rawIDs))
	for i, raw := range rawIDs {
		value, err := strconv.ParseUint(raw, 10, 64)
		if err != nil || strconv.FormatUint(value, 10) != raw || (i != 5 && value == 0) {
			return zero, fmt.Errorf("%w: invalid canonical search ID", ErrResponse)
		}
		ids[i] = value
	}

	if !utf8.ValidString(wire.Version) || strings.TrimSpace(wire.Version) == "" || len(wire.Version) > 256 {
		return zero, fmt.Errorf("%w: invalid search version", ErrResponse)
	}
	effective := time.UnixMilli(wire.EffectiveAt).UTC()
	if effective.Year() < 1 || effective.Year() > 9999 || wire.EffectiveAt > q.At.UnixMilli() {
		return zero, fmt.Errorf("%w: invalid search effective time", ErrResponse)
	}

	var expires *time.Time
	if wire.HasExpiresAt {
		value := time.UnixMilli(wire.ExpiresAt).UTC()
		if value.Year() < 1 || value.Year() > 9999 || wire.ExpiresAt <= wire.EffectiveAt || wire.ExpiresAt <= q.At.UnixMilli() {
			return zero, fmt.Errorf("%w: invalid search expiration", ErrResponse)
		}
		expires = &value
	} else if wire.ExpiresAt != 0 {
		return zero, fmt.Errorf("%w: inconsistent search expiration", ErrResponse)
	}

	if (q.Options.DocumentID != 0 && ids[1] != q.Options.DocumentID) || (q.Options.CategoryID != 0 && ids[4] != q.Options.CategoryID) {
		return zero, fmt.Errorf("%w: search hit violates filters", ErrResponse)
	}

	score := wire.Distance
	if math.IsNaN(score) || math.IsInf(score, 0) || score < -1-1e-4 || score > 1+1e-4 {
		return zero, fmt.Errorf("%w: invalid cosine score", ErrResponse)
	}
	score = max(-1.0, min(1.0, score))

	return SearchHit{
		ChunkID:             ids[0],
		DocumentID:          ids[1],
		VersionID:           ids[2],
		ArticleID:           ids[3],
		CategoryID:          ids[4],
		PublicationRevision: ids[5],
		Version:             wire.Version,
		EffectiveAt:         effective,
		ExpiresAt:           expires,
		Score:               score,
	}, nil
}
