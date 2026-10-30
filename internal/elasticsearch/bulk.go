package elasticsearch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"unicode/utf8"

	indexing "video-support-agent/internal/index"
)

const MaxBulkDocuments = 500

type bulkDocument struct {
	id     string
	action []byte
	source []byte
}

type bulkItemResponse struct {
	Index  string          `json:"_index"`
	ID     string          `json:"_id"`
	Status int             `json:"status"`
	Result string          `json:"result"`
	Error  json.RawMessage `json:"error"`
	Shards *struct {
		Failed *int `json:"failed"`
	} `json:"_shards"`
}

func (c *Client) BulkIndex(ctx context.Context, index string, documents []indexing.EncodedDocument) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !indexNamePattern.MatchString(index) || index == c.alias {
		return fmt.Errorf("%w: invalid bulk index name", ErrRejected)
	}

	prepared, err := prepareBulkDocuments(ctx, documents)
	if err != nil {
		return err
	}

	if err := c.requireUnaliasedIndex(ctx, index); err != nil {
		return err
	}

	if len(prepared) == 0 {
		return nil
	}

	path := "/" + url.PathEscape(index) + "/_bulk"
	completedBatches := 0

	for start := 0; start < len(prepared); {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("bulk stopped after %d completed batches: %w", completedBatches, err)
		}

		var body bytes.Buffer
		end := start

		for end < len(prepared) && end-start < MaxBulkDocuments {
			document := prepared[end]
			required := len(document.action) + 1 + len(document.source) + 1

			if body.Len()+required > MaxRequestBytes {
				break
			}

			body.Write(document.action)
			body.WriteByte('\n')
			body.Write(document.source)
			body.WriteByte('\n')
			end++
		}

		if end == start {
			return fmt.Errorf("%w: one bulk document exceeds request budget", ErrRejected)
		}

		status, response, err := c.request(ctx, http.MethodPost, path, "application/x-ndjson", body.Bytes())
		if err != nil {
			return fmt.Errorf("%w: bulk batch %d after %d completed batches: %w", ErrStateUnknown, completedBatches+1, completedBatches, err)
		}

		if status != http.StatusOK {
			return fmt.Errorf("%w: bulk batch %d returned HTTP %d after %d completed batches", ErrStateUnknown, completedBatches+1, status, completedBatches)
		}

		if err := verifyBulkResponse(
			index, prepared[start:end], response,
		); err != nil {
			return fmt.Errorf("bulk batch %d after %d completed batches: %w", completedBatches+1, completedBatches, err)
		}

		completedBatches++
		start = end
	}

	return nil
}

func prepareBulkDocuments(ctx context.Context, documents []indexing.EncodedDocument) ([]bulkDocument, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if documents == nil {
		return nil, fmt.Errorf("%w: bulk documents must not be nil", ErrRejected)
	}
	if len(documents) > indexing.MaxExportDocuments {
		return nil, fmt.Errorf("%w: too many bulk documents", ErrRejected)
	}

	prepared := make([]bulkDocument, 0, len(documents))
	seen := make(map[string]struct{}, len(documents))
	var totalJSONBytes int64

	for position, document := range documents {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		if len(document.ID) == 0 || len(document.ID) > 20 {
			return nil, fmt.Errorf("%w: invalid bulk ID at position %d", ErrRejected, position)
		}

		id, err := strconv.ParseUint(document.ID, 10, 64)
		if err != nil || id == 0 || strconv.FormatUint(id, 10) != document.ID {
			return nil, fmt.Errorf("%w: bulk ID must be canonical positive decimal at position %d", ErrRejected, position)
		}
		if _, exists := seen[document.ID]; exists {
			return nil, fmt.Errorf("%w: duplicate bulk ID %s", ErrRejected, document.ID)
		}
		seen[document.ID] = struct{}{}

		if len(document.JSON) == 0 || len(document.JSON) > indexing.MaxIndexDocumentJSONBytes {
			return nil, fmt.Errorf("%w: invalid JSON size for bulk ID %s", ErrRejected, document.ID)
		}

		totalJSONBytes += int64(len(document.JSON))
		if totalJSONBytes > indexing.MaxExportJSONBytes {
			return nil, fmt.Errorf("%w: bulk JSON total exceeds export budget", ErrRejected)
		}

		if !utf8.Valid(document.JSON) {
			return nil, fmt.Errorf("%w: invalid UTF-8 for bulk ID %s", ErrRejected, document.ID)
		}

		var object map[string]json.RawMessage
		if err := json.Unmarshal(document.JSON, &object); err != nil || object == nil {
			return nil, fmt.Errorf("%w: bulk source must be a JSON object for ID %s", ErrRejected, document.ID)
		}

		var chunkID string
		if err := json.Unmarshal(object["chunk_id"], &chunkID); err != nil || chunkID != document.ID {
			return nil, fmt.Errorf("%w: chunk_id does not match bulk ID %s", ErrRejected, document.ID)
		}

		var compact bytes.Buffer
		if err := json.Compact(&compact, document.JSON); err != nil {
			return nil, fmt.Errorf("%w: cannot compact source for bulk ID %s", ErrRejected, document.ID)
		}

		action := struct {
			Index struct {
				ID string `json:"_id"`
			} `json:"index"`
		}{}
		action.Index.ID = document.ID

		actionJSON, err := json.Marshal(action)
		if err != nil {
			return nil, fmt.Errorf("%w: cannot encode bulk action", ErrRejected)
		}

		source := compact.Bytes()
		if len(actionJSON)+1+len(source)+1 > MaxRequestBytes {
			return nil, fmt.Errorf("%w: bulk ID %s exceeds request budget", ErrRejected, document.ID)
		}

		prepared = append(prepared, bulkDocument{
			id:     document.ID,
			action: actionJSON,
			source: source,
		})
	}

	return prepared, nil
}

func (c *Client) requireUnaliasedIndex(ctx context.Context, index string) error {
	path := "/" + url.PathEscape(index) + "/_alias"
	status, body, err := c.request(ctx, http.MethodGet, path, "", nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("%w: bulk target lookup returned HTTP %d", ErrRejected, status)
	}

	var result map[string]struct {
		Aliases map[string]json.RawMessage `json:"aliases"`
	}
	if err := json.Unmarshal(body, &result); err != nil || len(result) != 1 {
		return fmt.Errorf("%w: bulk target must resolve to one physical index", ErrResponse)
	}

	entry, exists := result[index]
	if !exists || entry.Aliases == nil {
		return fmt.Errorf("%w: bulk target identity or aliases missing", ErrResponse)
	}
	if len(entry.Aliases) != 0 {
		return fmt.Errorf("%w: bulk target already has aliases", ErrRejected)
	}

	return nil
}

func verifyBulkResponse(index string, expected []bulkDocument, body []byte) error {
	var response struct {
		Errors *bool                         `json:"errors"`
		Items  []map[string]bulkItemResponse `json:"items"`
	}
	if err := json.Unmarshal(body, &response); err != nil || response.Errors == nil || len(response.Items) != len(expected) {
		return invalidBulkResponse("missing or malformed bulk envelope")
	}

	failures := 0
	firstFailureID := ""
	firstFailureStatus := 0

	for position, operations := range response.Items {
		item, exists := operations["index"]
		if !exists || len(operations) != 1 {
			return invalidBulkResponse("unexpected bulk operation")
		}
		if item.Index != index || item.ID != expected[position].id {
			return invalidBulkResponse("bulk item identity mismatch")
		}

		errorJSON := bytes.TrimSpace(item.Error)
		hasError := len(errorJSON) != 0 && !bytes.Equal(errorJSON, []byte("null"))

		if item.Status >= 400 && item.Status <= 599 {
			var details map[string]json.RawMessage
			if !hasError || json.Unmarshal(errorJSON, &details) != nil || len(details) == 0 {
				return invalidBulkResponse("failed item lacks error object")
			}

			failures++
			if firstFailureID == "" {
				firstFailureID = item.ID
				firstFailureStatus = item.Status
			}
			continue
		}

		validResult := (item.Status == http.StatusCreated && item.Result == "created") || (item.Status == http.StatusOK && item.Result == "updated")

		if hasError || !validResult {
			return invalidBulkResponse("unconfirmed bulk item result")
		}
		if item.Shards == nil || item.Shards.Failed == nil || *item.Shards.Failed != 0 {
			return invalidBulkResponse("bulk shard success not confirmed")
		}
	}

	if *response.Errors != (failures > 0) {
		return invalidBulkResponse("bulk errors flag contradicts items")
	}
	if failures > 0 {
		return fmt.Errorf("%w: %d bulk items failed; first ID %s, status %d", ErrRejected, failures, firstFailureID, firstFailureStatus)
	}

	return nil
}

func invalidBulkResponse(message string) error {
	return fmt.Errorf("%w: %w: %s", ErrStateUnknown, ErrResponse, message)
}
