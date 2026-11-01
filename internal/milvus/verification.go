package milvus

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sync/atomic"

	"video-support-agent/internal/embedding"
)

type VerificationReport struct {
	Collection   string                  `json:"collection"`
	CollectionID int64                   `json:"collection_id"`
	Count        int                     `json:"count"`
	Identity     embedding.ModelIdentity `json:"identity"`
}

type VerifiedTarget struct {
	client *Client
	report VerificationReport
	used   atomic.Bool
}

func (v *VerifiedTarget) Report() VerificationReport {
	return v.report
}

func (c *Client) VerifyCollection(ctx context.Context, build *BuildResult, prepared *PreparedImport) (*VerifiedTarget, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, ok := ctx.Deadline(); !ok {
		return nil, fmt.Errorf("%w: overall deadline is required", ErrInput)
	}
	if build == nil || prepared == nil || build.Stage != "ready-for-verification" || build.CollectionID <= 0 || !targetPattern.MatchString(build.Collection) || build.Inserted != len(prepared.Records) {
		return nil, fmt.Errorf("%w: invalid verification inputs", ErrInput)
	}

	if _, err := c.planBatches(
		ctx, build.Collection, prepared.Records,
	); err != nil {
		return nil, err
	}

	report := VerificationReport{
		Collection:   build.Collection,
		CollectionID: build.CollectionID,
		Count:        len(prepared.Records),
		Identity:     prepared.Identity,
	}
	if err := c.checkReady(ctx, report); err != nil {
		return nil, err
	}

	fields := make([]string, 0, 11)
	for _, field := range collectionFields() {
		fields = append(fields, field.Name)
	}

	for start := 0; start < len(prepared.Records); start += 64 {
		end := min(start+64, len(prepared.Records))
		expected := make(map[string]Record, end-start)
		ids := make([]string, 0, end-start)
		for _, record := range prepared.Records[start:end] {
			expected[record.ChunkID] = record
			ids = append(ids, record.ChunkID)
		}

		filterIDs, err := json.Marshal(ids)
		if err != nil {
			return nil, fmt.Errorf("%w: cannot encode ID filter", ErrInput)
		}
		data, err := c.request(ctx, "entities/query", map[string]any{
			"collectionName":   report.Collection,
			"filter":           "chunk_id in " + string(filterIDs),
			"outputFields":     fields,
			"limit":            len(ids),
			"consistencyLevel": "Strong",
		}, false)
		if err != nil {
			return nil, err
		}

		var rows []json.RawMessage
		if json.Unmarshal(data, &rows) != nil || len(rows) != len(ids) {
			return nil, fmt.Errorf("%w: query row count differs", ErrResponse)
		}
		for _, row := range rows {
			actual, err := decodeRecord(row)
			if err != nil {
				return nil, err
			}
			want, ok := expected[actual.ChunkID]
			if !ok {
				return nil, fmt.Errorf(
					"%w: unexpected or duplicate queried ID",
					ErrResponse,
				)
			}
			if err := compareRecord(actual, want); err != nil {
				return nil, fmt.Errorf("chunk_id=%s: %w", want.ChunkID, err)
			}
			delete(expected, actual.ChunkID)
		}
		if len(expected) != 0 {
			return nil, fmt.Errorf("%w: missing queried IDs", ErrResponse)
		}
	}

	seen := make(map[int]bool)
	for _, position := range []int{0, len(prepared.Records) / 2, len(prepared.Records) - 1} {
		if seen[position] {
			continue
		}
		seen[position] = true
		if err := c.verifySelfSearch(
			ctx, report.Collection, prepared.Records[position],
		); err != nil {
			return nil, err
		}
	}

	if err := c.checkReady(ctx, report); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &VerifiedTarget{client: c, report: report}, nil
}

func (c *Client) checkReady(ctx context.Context, report VerificationReport) error {
	id, err := c.VerifyCollectionContract(
		ctx, report.Collection, report.Identity,
	)
	if err != nil {
		return err
	}
	if id != report.CollectionID {
		return fmt.Errorf("%w: collection identity changed", ErrResponse)
	}

	ready, err := c.indexReady(ctx, report.Collection)
	if err != nil {
		return err
	}
	if !ready {
		return fmt.Errorf("%w: index is not ready", ErrUnavailable)
	}

	data, err := c.request(ctx, "collections/get_load_state", map[string]any{"collectionName": report.Collection}, false)
	if err != nil {
		return err
	}
	var state struct {
		LoadState string `json:"loadState"`
	}
	if json.Unmarshal(data, &state) != nil {
		return fmt.Errorf("%w: invalid load state", ErrResponse)
	}
	if state.LoadState != "LoadStateLoaded" {
		return fmt.Errorf("%w: collection is not loaded", ErrUnavailable)
	}

	data, err = c.request(ctx, "entities/query", map[string]any{
		"collectionName":   report.Collection,
		"filter":           "",
		"outputFields":     []string{"count(*)"},
		"consistencyLevel": "Strong",
	}, false)
	if err != nil {
		return err
	}
	var counts []struct {
		Count *int64 `json:"count(*)"`
	}
	if json.Unmarshal(data, &counts) != nil || len(counts) != 1 || counts[0].Count == nil || *counts[0].Count != int64(report.Count) {
		return fmt.Errorf("%w: collection count differs", ErrResponse)
	}
	return nil
}

func decodeRecord(data []byte) (Record, error) {
	var zero Record
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || len(fields) != 11 {
		return zero, fmt.Errorf("%w: invalid record fields", ErrResponse)
	}
	for _, field := range collectionFields() {
		value, exists := fields[field.Name]
		if !exists || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return zero, fmt.Errorf("%w: missing or null field", ErrResponse)
		}
	}

	var record Record
	if json.Unmarshal(data, &record) != nil {
		return zero, fmt.Errorf("%w: invalid record types", ErrResponse)
	}

	var vector []*float32
	if json.Unmarshal(fields["embedding"], &vector) != nil {
		return zero, fmt.Errorf("%w: invalid vector encoding", ErrResponse)
	}
	for _, value := range vector {
		if value == nil {
			return zero, fmt.Errorf("%w: null vector component", ErrResponse)
		}
	}
	if err := validateRecord(record); err != nil {
		return zero, fmt.Errorf("%w: invalid stored record", ErrResponse)
	}
	return record, nil
}

func compareRecord(actual, expected Record) error {
	if actual.ChunkID != expected.ChunkID ||
		actual.DocumentID != expected.DocumentID ||
		actual.VersionID != expected.VersionID ||
		actual.ArticleID != expected.ArticleID ||
		actual.Category != expected.Category ||
		actual.PublicationRevision != expected.PublicationRevision ||
		actual.Version != expected.Version ||
		actual.EffectiveAt != expected.EffectiveAt ||
		actual.HasExpiresAt != expected.HasExpiresAt ||
		actual.ExpiresAt != expected.ExpiresAt {
		return fmt.Errorf("%w: stored metadata differs", ErrResponse)
	}

	for i, value := range actual.Embedding {
		want := expected.Embedding[i]
		tolerance := 1e-6 * math.Max(1, math.Abs(float64(want)))
		if math.Abs(float64(value)-float64(want)) > tolerance {
			return fmt.Errorf("%w: stored vector differs", ErrResponse)
		}
	}
	return nil
}

func (c *Client) verifySelfSearch(ctx context.Context, name string, record Record) error {
	idJSON, err := json.Marshal(record.ChunkID)
	if err != nil {
		return fmt.Errorf("%w: cannot encode search filter", ErrInput)
	}
	data, err := c.request(ctx, "entities/search", map[string]any{
		"collectionName":   name,
		"annsField":        "embedding",
		"data":             [][]float32{record.Embedding},
		"filter":           "chunk_id == " + string(idJSON),
		"limit":            1,
		"outputFields":     []string{"chunk_id"},
		"consistencyLevel": "Strong",
		"searchParams": map[string]any{
			"metricType": "COSINE",
			"params":     map[string]any{"ef": 64},
		},
	}, false)
	if err != nil {
		return err
	}

	var hits []struct {
		ChunkID  string   `json:"chunk_id"`
		Distance *float64 `json:"distance"`
	}
	if json.Unmarshal(data, &hits) != nil || len(hits) != 1 || hits[0].ChunkID != record.ChunkID || hits[0].Distance == nil {
		return fmt.Errorf("%w: self-search result differs", ErrResponse)
	}
	score := *hits[0].Distance
	if math.IsNaN(score) || math.IsInf(score, 0) || math.Abs(score-1) > 1e-4 {
		return fmt.Errorf("%w: self-search cosine differs", ErrResponse)
	}
	return nil
}
