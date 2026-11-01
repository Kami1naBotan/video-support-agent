package milvus

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"video-support-agent/internal/embedding"
	indexing "video-support-agent/internal/index"
)

type BuildResult struct {
	Collection   string
	CollectionID int64
	Stage        string
	Inserted     int
}

type batchRange struct {
	Start int
	End   int
}

func (c *Client) BuildCollection(ctx context.Context, name string, prepared *PreparedImport) (*BuildResult, error) {
	result := &BuildResult{Collection: name, Stage: "preflight"}

	if err := ctx.Err(); err != nil {
		return result, err
	}
	if _, ok := ctx.Deadline(); !ok {
		return result, fmt.Errorf("%w: overall deadline is required", ErrInput)
	}
	if !targetPattern.MatchString(name) || name == c.alias {
		return result, fmt.Errorf("%w: invalid new collection name", ErrInput)
	}
	if prepared == nil {
		return result, fmt.Errorf("%w: prepared import is required", ErrInput)
	}

	description, err := contractDescription(prepared.Identity)
	if err != nil {
		return result, err
	}
	batches, err := c.planBatches(ctx, name, prepared.Records)
	if err != nil {
		return result, err
	}
	if err := c.ensureUnused(ctx, name); err != nil {
		return result, err
	}

	result.Stage = "create"
	if err := c.createCollection(ctx, name, description); err != nil {
		return result, err
	}

	result.Stage = "schema-check"
	id, err := c.VerifyCollectionContract(ctx, name, prepared.Identity)
	if err != nil {
		return result, err
	}
	result.CollectionID = id

	result.Stage = "insert"
	for _, batch := range batches {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		records := prepared.Records[batch.Start:batch.End]
		if err := c.insertBatch(ctx, name, records); err != nil {
			return result, err
		}
		result.Inserted += len(records)
	}

	result.Stage = "flush"
	if _, err := c.request(ctx, "collections/flush", map[string]any{"collectionName": name}, true); err != nil {
		return result, err
	}

	result.Stage = "index"
	if _, err := c.request(ctx, "indexes/create", map[string]any{
		"collectionName": name,
		"indexParams": []map[string]any{{
			"fieldName":  "embedding",
			"indexName":  "embedding_hnsw",
			"indexType":  "HNSW",
			"metricType": "COSINE",
			"params": map[string]string{
				"M": "16", "efConstruction": "200",
			},
		}},
	}, true); err != nil {
		return result, err
	}
	if err := poll(ctx, func() (bool, error) {
		return c.indexReady(ctx, name)
	}); err != nil {
		return result, err
	}

	result.Stage = "load"
	if _, err := c.request(ctx, "collections/load", map[string]any{"collectionName": name}, true); err != nil {
		return result, err
	}
	if err := poll(ctx, func() (bool, error) {
		data, err := c.request(ctx, "collections/get_load_state", map[string]any{
			"collectionName": name,
		}, false)
		if err != nil {
			return false, err
		}
		var state struct {
			LoadState string `json:"loadState"`
		}
		if json.Unmarshal(data, &state) != nil {
			return false, fmt.Errorf("%w: invalid load state", ErrResponse)
		}
		switch state.LoadState {
		case "LoadStateLoaded":
			return true, nil
		case "LoadStateLoading", "LoadStateNotLoad":
			return false, nil
		default:
			return false, fmt.Errorf("%w: unexpected load state", ErrResponse)
		}
	}); err != nil {
		return result, err
	}

	if err := ctx.Err(); err != nil {
		return result, err
	}
	result.Stage = "ready-for-verification"
	return result, nil
}

func (c *Client) planBatches(ctx context.Context, name string, records []Record) ([]batchRange, error) {
	if len(records) == 0 || len(records) > indexing.MaxExportDocuments {
		return nil, fmt.Errorf("%w: invalid record count", ErrInput)
	}

	empty, err := json.Marshal(map[string]any{
		"dbName": c.database, "collectionName": name, "data": []Record{},
	})
	if err != nil {
		return nil, fmt.Errorf("%w: cannot encode batch envelope", ErrInput)
	}
	baseBytes := len(empty)
	seen := make(map[string]struct{}, len(records))
	batches := make([]batchRange, 0)
	start, used := 0, baseBytes

	for i, record := range records {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := validateRecord(record); err != nil {
			return nil, fmt.Errorf("record[%d]: %w", i, err)
		}
		if _, exists := seen[record.ChunkID]; exists {
			return nil, fmt.Errorf("%w: duplicate chunk ID", ErrInput)
		}
		seen[record.ChunkID] = struct{}{}

		encoded, err := json.Marshal(record)
		if err != nil {
			return nil, fmt.Errorf("%w: cannot encode record", ErrInput)
		}
		if len(encoded) > maxRequestBytes-baseBytes {
			return nil, fmt.Errorf("%w: record exceeds request budget", ErrInput)
		}

		extra := len(encoded)
		if i > start {
			extra++
		}
		if i-start >= 64 || extra > maxRequestBytes-used {
			batches = append(batches, batchRange{Start: start, End: i})
			start, used = i, baseBytes
			extra = len(encoded)
		}
		used += extra
	}
	return append(batches, batchRange{Start: start, End: len(records)}), nil
}

func validateRecord(record Record) error {
	for i, id := range []string{record.ChunkID, record.DocumentID, record.VersionID, record.ArticleID, record.Category, record.PublicationRevision} {
		value, err := strconv.ParseUint(id, 10, 64)
		if err != nil || strconv.FormatUint(value, 10) != id || (i != 5 && value == 0) {
			return fmt.Errorf("%w: invalid canonical ID", ErrInput)
		}
	}

	if !utf8.ValidString(record.Version) || strings.TrimSpace(record.Version) == "" || len(record.Version) > 256 {
		return fmt.Errorf("%w: invalid version", ErrInput)
	}
	if (!record.HasExpiresAt && record.ExpiresAt != 0) || (record.HasExpiresAt && record.ExpiresAt <= record.EffectiveAt) {
		return fmt.Errorf("%w: inconsistent expiration", ErrInput)
	}
	if len(record.Embedding) != embedding.Dimensions {
		return fmt.Errorf("%w: invalid vector dimensions", ErrInput)
	}
	nonzero := false
	for _, value := range record.Embedding {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return fmt.Errorf("%w: non-finite vector component", ErrInput)
		}
		nonzero = nonzero || value != 0
	}
	if !nonzero {
		return fmt.Errorf("%w: zero vector", ErrInput)
	}
	return nil
}

func (c *Client) insertBatch(ctx context.Context, name string, records []Record) error {
	data, err := c.request(ctx, "entities/insert", map[string]any{
		"collectionName": name,
		"data":           records,
	}, true)
	if err != nil {
		return err
	}

	var response struct {
		InsertCount *int     `json:"insertCount"`
		InsertIDs   []string `json:"insertIds"`
	}
	if json.Unmarshal(data, &response) != nil || response.InsertCount == nil || *response.InsertCount != len(records) || len(response.InsertIDs) != len(records) {
		return operationError(true, fmt.Errorf("%w: insert acknowledgement differs", ErrResponse))
	}

	expected := make(map[string]struct{}, len(records))
	for _, record := range records {
		expected[record.ChunkID] = struct{}{}
	}
	for _, id := range response.InsertIDs {
		if _, ok := expected[id]; !ok {
			return operationError(true, fmt.Errorf("%w: unexpected or duplicate inserted ID", ErrResponse))
		}
		delete(expected, id)
	}
	if len(expected) != 0 {
		return operationError(true, fmt.Errorf("%w: missing inserted IDs", ErrResponse))
	}
	return nil
}

func (c *Client) indexReady(ctx context.Context, name string) (bool, error) {
	data, err := c.request(ctx, "indexes/describe", map[string]any{
		"collectionName": name,
		"indexName":      "embedding_hnsw",
	}, false)
	if err != nil {
		return false, err
	}
	var indexes []struct {
		FieldName   string `json:"fieldName"`
		IndexName   string `json:"indexName"`
		IndexType   string `json:"indexType"`
		MetricType  string `json:"metricType"`
		IndexState  string `json:"indexState"`
		IndexParams []struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		} `json:"indexParams"`
	}
	if json.Unmarshal(data, &indexes) != nil || len(indexes) != 1 {
		return false, fmt.Errorf("%w: invalid index description", ErrResponse)
	}
	index := indexes[0]
	if index.FieldName != "embedding" || index.IndexName != "embedding_hnsw" || index.IndexType != "HNSW" || index.MetricType != "COSINE" {
		return false, fmt.Errorf("%w: index contract differs", ErrResponse)
	}

	found := 0
	for _, parameter := range index.IndexParams {
		if parameter.Key != "params" {
			continue
		}
		found++
		var values struct {
			M              string `json:"M"`
			EfConstruction string `json:"efConstruction"`
		}
		if json.Unmarshal([]byte(parameter.Value), &values) != nil || values.M != "16" || values.EfConstruction != "200" {
			return false, fmt.Errorf("%w: index parameters differ", ErrResponse)
		}
	}
	if found != 1 {
		return false, fmt.Errorf("%w: missing or duplicate index parameters", ErrResponse)
	}

	switch index.IndexState {
	case "Finished":
		return true, nil
	case "Unissued", "InProgress":
		return false, nil
	case "Failed":
		return false, fmt.Errorf("%w: index construction failed", ErrRejected)
	default:
		return false, fmt.Errorf("%w: unexpected index state", ErrResponse)
	}
}

func poll(ctx context.Context, check func() (bool, error)) error {
	for attempt := 0; attempt < 120; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		ready, err := check()
		if err != nil {
			return err
		}
		if ready {
			return ctx.Err()
		}
		if attempt == 119 {
			break
		}
		timer := time.NewTimer(2 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return fmt.Errorf("%w: readiness polling budget exhausted", ErrUnavailable)
}
