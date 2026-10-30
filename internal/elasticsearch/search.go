package elasticsearch

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"video-support-agent/internal/retrieval"
)

var _ retrieval.CandidateSearcher = (*Client)(nil)

func (c *Client) SearchCandidates(ctx context.Context, query string, opts retrieval.SearchOptions, at time.Time) ([]retrieval.Candidate, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	query, opts, err := retrieval.NormalizeSearch(query, opts)
	if err != nil {
		return nil, err
	}

	at = at.UTC().Truncate(time.Millisecond)
	body, err := BuildSearchQuery(query, opts, at)
	if err != nil {
		return nil, err
	}

	target, err := c.GetAliasTarget(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve search alias: %w", err)
	}
	if target == "" {
		return nil, retrieval.ErrIndexNotReady
	}

	path := "/" + url.PathEscape(target) + "/_search?allow_partial_search_results=false"

	status, data, err := c.request(ctx, http.MethodPost, path, "application/json", body)
	if err != nil {
		return nil, fmt.Errorf("search candidates: %w", err)
	}

	switch {
	case status == http.StatusNotFound:
		return nil, retrieval.ErrIndexNotReady
	case status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500:
		return nil, fmt.Errorf("%w: search returned HTTP %d", ErrUnavailable, status)
	case status != http.StatusOK:
		return nil, fmt.Errorf("%w: search returned HTTP %d", ErrRejected, status)
	}

	candidates, err := decodeSearchCandidates(ctx, data, target, opts, at)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return candidates, nil
}
