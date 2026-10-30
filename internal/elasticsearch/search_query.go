package elasticsearch

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"video-support-agent/internal/retrieval"
)

func BuildSearchQuery(query string, opts retrieval.SearchOptions, at time.Time) ([]byte, error) {
	query, opts, err := retrieval.NormalizeSearch(query, opts)
	if err != nil {
		return nil, err
	}

	at = at.UTC().Truncate(time.Millisecond)
	if at.IsZero() || at.Year() < 1 || at.Year() > 9999 {
		return nil, fmt.Errorf("%w: a valid query time is required", retrieval.ErrInvalidQuery)
	}
	timestamp := at.Format("2006-01-02T15:04:05.000Z")

	candidateLimit := opts.Limit * retrieval.CandidateMultiplier
	if candidateLimit < 1 || candidateLimit > retrieval.MaxCandidates {
		return nil, fmt.Errorf("%w: candidate limit is out of range", retrieval.ErrInvalidQuery)
	}

	filters := []any{
		searchTerm("is_current", true),
		searchTerm("status", "published"),
		map[string]any{
			"range": map[string]any{
				"effective_at": map[string]any{
					"lte": timestamp,
				},
			},
		},
		map[string]any{
			"bool": map[string]any{
				"should": []any{
					map[string]any{
						"bool": map[string]any{
							"must_not": []any{
								searchExists("expires_at"),
							},
						},
					},
					map[string]any{
						"range": map[string]any{
							"expires_at": map[string]any{
								"gt": timestamp,
							},
						},
					},
				},
				"minimum_should_match": 1,
			},
		},
	}

	if opts.DocumentID != 0 {
		filters = append(filters, searchTerm("document_id", strconv.FormatUint(opts.DocumentID, 10)))
	}
	if opts.CategoryID != 0 {
		filters = append(filters, searchTerm("category_id", strconv.FormatUint(opts.CategoryID, 10)))
	}
	if opts.ArticleNumber != "" {
		filters = append(filters, searchTerm("article_number", opts.ArticleNumber))
	}

	highlightClauses := make([]any, 0, 2)
	var textQuery map[string]any

	if query != "" {
		textQuery = map[string]any{
			"multi_match": map[string]any{
				"query":    query,
				"fields":   []string{"title^3", "section_path^2", "content"},
				"type":     "best_fields",
				"operator": "or",
			},
		}
		highlightClauses = append(highlightClauses, textQuery)
	}

	if opts.Section != "" {
		sectionQuery := map[string]any{
			"match": map[string]any{
				"section_path": map[string]any{
					"query":    opts.Section,
					"operator": "or",
				},
			},
		}
		filters = append(filters, sectionQuery)
		highlightClauses = append(highlightClauses, sectionQuery)
	}

	booleanQuery := map[string]any{
		"filter": filters,
		"must_not": []any{
			searchExists("withdrawn_at"),
		},
	}
	if textQuery != nil {
		booleanQuery["must"] = []any{textQuery}
	}

	body := map[string]any{
		"size":             candidateLimit,
		"track_total_hits": false,
		"_source": []string{
			"chunk_id",
			"document_id",
			"version_id",
			"article_id",
			"category_id",
			"publication_revision",
			"chunk_index",
			"article_order",
			"version",
			"title",
			"section_path",
			"article_number",
			"content",
			"source_url",
			"status",
			"is_current",
			"effective_at",
			"expires_at",
			"withdrawn_at",
		},
		"query": map[string]any{
			"bool": booleanQuery,
		},
		"sort": []any{
			map[string]any{"_score": "desc"},
			map[string]any{"chunk_id": "asc"},
		},
	}

	if opts.Highlight {
		highlightQuery := map[string]any{
			"match_none": map[string]any{},
		}
		if len(highlightClauses) != 0 {
			highlightQuery = map[string]any{
				"bool": map[string]any{
					"should":               highlightClauses,
					"minimum_should_match": 1,
				},
			}
		}

		body["highlight"] = map[string]any{
			"type":                "unified",
			"encoder":             "html",
			"pre_tags":            []string{"<em>"},
			"post_tags":           []string{"</em>"},
			"require_field_match": true,
			"highlight_query":     highlightQuery,
			"fields": map[string]any{
				"title": map[string]any{
					"number_of_fragments": 0,
					"no_match_size":       0,
				},
				"section_path": map[string]any{
					"number_of_fragments": 2,
					"fragment_size":       120,
					"no_match_size":       0,
				},
				"content": map[string]any{
					"number_of_fragments": 3,
					"fragment_size":       160,
					"no_match_size":       0,
				},
			},
		}
	}

	data, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("%w: cannot encode search request", ErrRejected)
	}
	if len(data) > MaxRequestBytes {
		return nil, fmt.Errorf("%w: search request exceeds the allowed size", ErrRejected)
	}

	return data, nil
}

func searchTerm(field string, value any) map[string]any {
	return map[string]any{
		"term": map[string]any{
			field: value,
		},
	}
}

func searchExists(field string) map[string]any {
	return map[string]any{
		"exists": map[string]any{
			"field": field,
		},
	}
}
