package elasticsearch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
)

var indexNamePattern = regexp.MustCompile(
	`^[a-z0-9][a-z0-9_-]{0,254}$`,
)

func (c *Client) GetAliasTarget(ctx context.Context) (string, error) {
	path := "/_alias/" + url.PathEscape(c.alias)
	status, body, err := c.request(ctx, http.MethodGet, path, "", nil)
	if err != nil {
		return "", err
	}

	if status == http.StatusNotFound {
		var missing struct {
			Error  json.RawMessage `json:"error"`
			Status int             `json:"status"`
		}
		if json.Unmarshal(body, &missing) != nil || missing.Status != http.StatusNotFound || len(missing.Error) == 0 || bytes.Equal(bytes.TrimSpace(missing.Error), []byte("null")) {
			return "", fmt.Errorf("%w: malformed alias-not-found response", ErrResponse)
		}
		return "", nil
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("%w: read alias returned HTTP %d", ErrRejected, status)
	}

	var result map[string]struct {
		Aliases map[string]struct {
			IsWriteIndex  *bool           `json:"is_write_index"`
			Filter        json.RawMessage `json:"filter"`
			Routing       string          `json:"routing"`
			IndexRouting  string          `json:"index_routing"`
			SearchRouting string          `json:"search_routing"`
		} `json:"aliases"`
	}

	if json.Unmarshal(body, &result) != nil || len(result) != 1 {
		return "", fmt.Errorf("%w: alias must have exactly one target", ErrResponse)
	}

	for index, entry := range result {
		if !indexNamePattern.MatchString(index) {
			return "", fmt.Errorf("%w: unsupported alias target name", ErrResponse)
		}

		options, exists := entry.Aliases[c.alias]
		if !exists || options.IsWriteIndex == nil || *options.IsWriteIndex {
			return "", fmt.Errorf("%w: alias must explicitly disable writes", ErrResponse)
		}

		if len(options.Filter) != 0 || options.Routing != "" || options.IndexRouting != "" || options.SearchRouting != "" {
			return "", fmt.Errorf("%w: filtered or routed alias is not supported", ErrResponse)
		}

		return index, nil
	}

	return "", fmt.Errorf("%w: missing alias target", ErrResponse)
}

func (c *Client) CreateIndex(ctx context.Context, index string, definition []byte) error {
	if !indexNamePattern.MatchString(index) || index == c.alias {
		return fmt.Errorf("%w: invalid physical index name", ErrRejected)
	}
	if len(definition) == 0 || len(definition) > MaxRequestBytes {
		return fmt.Errorf("%w: invalid index definition size", ErrRejected)
	}

	var object map[string]json.RawMessage
	if err := json.Unmarshal(definition, &object); err != nil || object == nil {
		return fmt.Errorf("%w: index definition must be a JSON object", ErrRejected)
	}
	if _, exists := object["aliases"]; exists {
		return fmt.Errorf("%w: index creation must not publish aliases", ErrRejected)
	}

	for _, key := range []string{"settings", "mappings"} {
		var value map[string]json.RawMessage
		if err := json.Unmarshal(object[key], &value); err != nil || value == nil {
			return fmt.Errorf("%w: index definition requires %s object", ErrRejected, key)
		}
	}

	path := "/" + url.PathEscape(index) + "?wait_for_active_shards=1"
	status, body, err := c.request(ctx, http.MethodPut, path, "application/json", definition)
	if err != nil {
		return fmt.Errorf("%w: create index: %w", ErrStateUnknown, err)
	}

	if status != http.StatusOK {
		if status >= 500 || status == http.StatusRequestTimeout {
			return fmt.Errorf("%w: create index returned HTTP %d", ErrStateUnknown, status)
		}
		return fmt.Errorf("%w: create index returned HTTP %d", ErrRejected, status)
	}

	var result struct {
		Acknowledged       *bool  `json:"acknowledged"`
		ShardsAcknowledged *bool  `json:"shards_acknowledged"`
		Index              string `json:"index"`
	}

	if json.Unmarshal(body, &result) != nil || result.Index != index || result.Acknowledged == nil || result.ShardsAcknowledged == nil || !*result.Acknowledged || !*result.ShardsAcknowledged {
		return fmt.Errorf("%w: index creation or shard readiness not confirmed", ErrStateUnknown)
	}

	return nil
}
