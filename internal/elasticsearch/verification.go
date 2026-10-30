package elasticsearch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"reflect"

	indexing "video-support-agent/internal/index"
)

type verificationShards struct {
	Total      *int `json:"total"`
	Successful *int `json:"successful"`
	Failed     *int `json:"failed"`
}

func (s *verificationShards) complete() bool {
	return s != nil &&
		s.Total != nil &&
		s.Successful != nil &&
		s.Failed != nil &&
		*s.Total > 0 &&
		*s.Successful == *s.Total &&
		*s.Failed == 0
}

func (c *Client) VerifyIndex(ctx context.Context, index string, documents []indexing.EncodedDocument) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !indexNamePattern.MatchString(index) || index == c.alias {
		return fmt.Errorf("%w: invalid verification index", ErrRejected)
	}

	prepared, err := prepareBulkDocuments(ctx, documents)
	if err != nil {
		return err
	}
	if err := c.requireUnaliasedIndex(ctx, index); err != nil {
		return err
	}

	basePath := "/" + url.PathEscape(index)

	status, body, err := c.request(ctx, http.MethodPost, basePath+"/_refresh", "", nil)
	if err != nil {
		return fmt.Errorf("refresh index: %w", err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("%w: refresh returned HTTP %d", ErrRejected, status)
	}

	var refresh struct {
		Shards *verificationShards `json:"_shards"`
	}
	if json.Unmarshal(body, &refresh) != nil || !refresh.Shards.complete() {
		return fmt.Errorf("%w: refresh shard success not confirmed", ErrResponse)
	}

	status, body, err = c.request(ctx, http.MethodGet, basePath+"/_count", "", nil)
	if err != nil {
		return fmt.Errorf("count index: %w", err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("%w: count returned HTTP %d", ErrRejected, status)
	}

	var count struct {
		Count  *int64              `json:"count"`
		Shards *verificationShards `json:"_shards"`
	}
	if json.Unmarshal(body, &count) != nil || count.Count == nil || *count.Count < 0 || !count.Shards.complete() {
		return fmt.Errorf("%w: malformed or partial count response", ErrResponse)
	}
	if *count.Count != int64(len(prepared)) {
		return fmt.Errorf("%w: index count mismatch: expected %d, got %d", ErrRejected, len(prepared), *count.Count)
	}

	if len(prepared) == 0 {
		return nil
	}

	positions := []int{0, len(prepared) / 2, len(prepared) - 1}
	seen := make(map[int]struct{}, len(positions))

	for _, position := range positions {
		if _, exists := seen[position]; exists {
			continue
		}
		seen[position] = struct{}{}

		if err := ctx.Err(); err != nil {
			return err
		}

		document := prepared[position]
		path := basePath + "/_doc/" + url.PathEscape(document.id) + "?realtime=false"

		status, body, err := c.request(ctx, http.MethodGet, path, "", nil)
		if err != nil {
			return fmt.Errorf("read verification sample: %w", err)
		}
		if status != http.StatusOK {
			return fmt.Errorf("%w: sample read returned HTTP %d for ID %s", ErrRejected, status, document.id)
		}

		var sample struct {
			Index  string          `json:"_index"`
			ID     string          `json:"_id"`
			Found  *bool           `json:"found"`
			Source json.RawMessage `json:"_source"`
		}
		if json.Unmarshal(body, &sample) != nil || sample.Index != index || sample.ID != document.id || sample.Found == nil || !*sample.Found {
			return fmt.Errorf("%w: sample identity or presence not confirmed", ErrResponse)
		}

		expected, err := decodeVerificationObject(document.source)
		if err != nil {
			return fmt.Errorf("%w: invalid expected sample JSON", ErrRejected)
		}
		actual, err := decodeVerificationObject(sample.Source)
		if err != nil {
			return fmt.Errorf("%w: invalid sample source JSON", ErrResponse)
		}
		if !reflect.DeepEqual(expected, actual) {
			return fmt.Errorf("%w: sample source mismatch for ID %s", ErrRejected, document.id)
		}
	}

	return nil
}

func decodeVerificationObject(data []byte) (map[string]any, error) {
	if !json.Valid(data) {
		return nil, fmt.Errorf("invalid JSON")
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()

	var object map[string]any
	if err := decoder.Decode(&object); err != nil {
		return nil, err
	}
	if object == nil {
		return nil, fmt.Errorf("JSON object required")
	}

	return object, nil
}
