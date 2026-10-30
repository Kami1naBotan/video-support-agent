package retrieval

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

var ErrInvalidQuery = errors.New("invalid retrieval query")

func NormalizeSearch(query string, opts SearchOptions) (string, SearchOptions, error) {
	normalizedQuery, err := normalizeSearchText("query", query, MaxQueryRunes)
	if err != nil {
		return "", SearchOptions{}, err
	}

	section, err := normalizeSearchText("section", opts.Section, MaxSectionRunes)
	if err != nil {
		return "", SearchOptions{}, err
	}

	articleNumber, err := normalizeSearchText("article_number", opts.ArticleNumber, MaxArticleNumberRunes)
	if err != nil {
		return "", SearchOptions{}, err
	}

	if normalizedQuery == "" && section == "" && articleNumber == "" {
		return "", SearchOptions{}, fmt.Errorf("%w: query, section or article_number is required", ErrInvalidQuery)
	}

	if opts.Limit < 0 || opts.Limit > MaxLimit {
		return "", SearchOptions{}, fmt.Errorf("%w: limit must be zero or between 1 and %d", ErrInvalidQuery, MaxLimit)
	}

	normalized := opts
	normalized.Section = section
	normalized.ArticleNumber = articleNumber

	if normalized.Limit == 0 {
		normalized.Limit = DefaultLimit
	}

	return normalizedQuery, normalized, nil
}

func normalizeSearchText(field string, value string, maxRunes int) (string, error) {
	if !utf8.ValidString(value) {
		return "", fmt.Errorf("%w: %s must contain valid UTF-8", ErrInvalidQuery, field)
	}

	normalized := strings.TrimSpace(value)

	if utf8.RuneCountInString(normalized) > maxRunes {
		return "", fmt.Errorf("%w: %s exceeds %d Unicode characters", ErrInvalidQuery, field, maxRunes)
	}

	return normalized, nil
}
