package retrieval

import "fmt"

const MaxHybridQueryBytes = 64 << 10

func NormalizeHybridSearch(query string, opts HybridOptions) (string, HybridOptions, error) {
	if len(query) > MaxHybridQueryBytes {
		return "", HybridOptions{}, fmt.Errorf("%w: hybrid query exceeds byte budget", ErrInvalidQuery)
	}

	normalizedQuery, searchOpts, err := NormalizeSearch(query, opts.searchOptions())
	if err != nil {
		return "", HybridOptions{}, err
	}

	normalizedOpts := opts
	normalizedOpts.TopK = searchOpts.Limit

	return normalizedQuery, normalizedOpts, nil
}

func (opts HybridOptions) searchOptions() SearchOptions {
	return SearchOptions{
		DocumentID: opts.DocumentID,
		VersionID:  opts.VersionID,
		CategoryID: opts.CategoryID,
		Limit:      opts.TopK,
	}
}
