package evaluation

import (
	"fmt"
	"net/url"
	"time"
)

const MaxCorpusArticles = 1000

type CorpusArticle struct {
	Key         CitationKey `json:"key"`
	CategoryKey string      `json:"category_key"`
	Title       string      `json:"title"`
	Content     string      `json:"content"`
	Scope       string      `json:"scope"`
	SourceURL   string      `json:"source_url"`
	EffectiveAt string      `json:"effective_at"`
	ExpiresAt   string      `json:"expires_at"`
}

type CorpusManifest struct {
	SchemaVersion int             `json:"schema_version"`
	CorpusID      string          `json:"corpus_id"`
	Synthetic     bool            `json:"synthetic"`
	Articles      []CorpusArticle `json:"articles"`
}

type Corpus struct {
	id         string
	digest     string
	synthetic  bool
	articles   map[CitationKey]CorpusArticle
	documents  map[string]struct{}
	categories map[string]struct{}
	versions   map[string]CorpusArticle
}

func (c *Corpus) ID() string {
	return c.id
}

func (c *Corpus) Digest() string {
	return c.digest
}

func (c *Corpus) Synthetic() bool {
	return c.synthetic
}

func (c *Corpus) Article(key CitationKey) (CorpusArticle, bool) {
	article, exists := c.articles[key]
	return article, exists
}

func corpusVersionKey(documentKey, versionKey string) string {
	return documentKey + "/" + versionKey
}

func NewCorpus(manifest CorpusManifest) (*Corpus, error) {
	if manifest.SchemaVersion != SchemaVersion {
		return nil, fieldError("corpus.schema_version", "unsupported version")
	}
	if err := validateKey("corpus.corpus_id", manifest.CorpusID); err != nil {
		return nil, err
	}
	if len(manifest.Articles) == 0 || len(manifest.Articles) > MaxCorpusArticles {
		return nil, fieldError("corpus.articles", "invalid article count")
	}

	corpus := &Corpus{
		id:         manifest.CorpusID,
		synthetic:  manifest.Synthetic,
		articles:   make(map[CitationKey]CorpusArticle),
		documents:  make(map[string]struct{}),
		categories: make(map[string]struct{}),
		versions:   make(map[string]CorpusArticle),
	}

	totalBytes := 0
	for i, article := range manifest.Articles {
		path := fmt.Sprintf("corpus.articles[%d]", i)
		if err := validateCorpusArticle(article, manifest.Synthetic); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}

		totalBytes += len(article.Content) + len(article.Title) +
			len(article.Scope) + len(article.SourceURL) +
			len(article.Key.DocumentKey) + len(article.Key.VersionKey) +
			len(article.Key.ArticleKey) + len(article.CategoryKey) +
			len(article.EffectiveAt) + len(article.ExpiresAt)
		if totalBytes > MaxDatasetBytes {
			return nil, fieldError("corpus", "content exceeds size limit")
		}

		if _, exists := corpus.articles[article.Key]; exists {
			return nil, fieldError(path, "duplicate article identity")
		}

		versionKey := corpusVersionKey(article.Key.DocumentKey, article.Key.VersionKey)
		if previous, exists := corpus.versions[versionKey]; exists {
			if previous.CategoryKey != article.CategoryKey ||
				previous.Title != article.Title ||
				previous.SourceURL != article.SourceURL ||
				previous.EffectiveAt != article.EffectiveAt ||
				previous.ExpiresAt != article.ExpiresAt {
				return nil, fieldError(path, "conflicting version metadata")
			}
		}

		corpus.articles[article.Key] = article
		corpus.documents[article.Key.DocumentKey] = struct{}{}
		corpus.categories[article.CategoryKey] = struct{}{}
		corpus.versions[versionKey] = article
	}

	digest, err := EvidenceHash(manifest)
	if err != nil {
		return nil, err
	}
	corpus.digest = digest
	return corpus, nil
}

func validateCorpusArticle(article CorpusArticle, synthetic bool) error {
	for _, item := range []struct {
		path  string
		value string
	}{
		{"key.document_key", article.Key.DocumentKey},
		{"key.version_key", article.Key.VersionKey},
		{"key.article_key", article.Key.ArticleKey},
		{"category_key", article.CategoryKey},
	} {
		if err := validateKey(item.path, item.value); err != nil {
			return err
		}
	}

	for _, item := range []struct {
		path  string
		value string
		limit int
	}{
		{"title", article.Title, 512},
		{"content", article.Content, MaxReferenceBytes},
		{"scope", article.Scope, 4096},
	} {
		if err := validateText(item.path, item.value, item.limit); err != nil {
			return err
		}
	}

	if article.SourceURL == "" {
		if !synthetic {
			return fieldError("source_url", "required for non-synthetic corpus")
		}
	} else {
		if err := validateText("source_url", article.SourceURL, 2048); err != nil {
			return err
		}
		parsed, err := url.Parse(article.SourceURL)
		if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil {
			return fieldError("source_url", "requires an HTTPS URL without user info")
		}
	}

	if len(article.EffectiveAt) > 64 || len(article.ExpiresAt) > 64 {
		return fieldError("validity", "timestamp is too long")
	}
	effective, err := time.Parse(time.RFC3339, article.EffectiveAt)
	if err != nil {
		return fieldError("effective_at", "requires an RFC3339 timestamp")
	}
	if article.ExpiresAt != "" {
		expires, err := time.Parse(time.RFC3339, article.ExpiresAt)
		if err != nil || !expires.After(effective) {
			return fieldError("expires_at", "must be later than effective_at")
		}
	}
	return nil
}

func (c *Corpus) ResolveFilterKeys(filters Filters) (Filters, error) {
	if c == nil {
		return Filters{}, fieldError("corpus", "is required")
	}
	if filters.DocumentKey != "" {
		if _, exists := c.documents[filters.DocumentKey]; !exists {
			return Filters{}, fieldError("filters.document_key", "unknown document")
		}
	}
	if filters.CategoryKey != "" {
		if _, exists := c.categories[filters.CategoryKey]; !exists {
			return Filters{}, fieldError("filters.category_key", "unknown category")
		}
	}
	if filters.VersionKey == "" {
		return filters, nil
	}

	if filters.DocumentKey != "" {
		key := corpusVersionKey(filters.DocumentKey, filters.VersionKey)
		if _, exists := c.versions[key]; !exists {
			return Filters{}, fieldError("filters.version_key", "unknown version")
		}
		return filters, nil
	}

	matches := 0
	for _, article := range c.versions {
		if article.Key.VersionKey == filters.VersionKey {
			matches++
			filters.DocumentKey = article.Key.DocumentKey
		}
	}
	if matches != 1 {
		return Filters{}, fieldError("filters.version_key", "requires an unambiguous document")
	}
	return filters, nil
}

func (c *Corpus) ValidateCases(cases []Case) error {
	if c == nil {
		return fieldError("corpus", "is required")
	}
	if len(cases) > MaxCases {
		return fieldError("cases", "too many samples")
	}

	seen := make(map[string]struct{}, len(cases))
	for i, sample := range cases {
		path := fmt.Sprintf("cases[%d]", i)
		if err := ValidateCase(sample); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if _, exists := seen[sample.ID]; exists {
			return fieldError(path, "duplicate case ID")
		}
		seen[sample.ID] = struct{}{}

		if sample.CorpusID != c.id {
			return fieldError(path+".corpus_id", "does not match corpus")
		}
		if _, err := c.ResolveFilterKeys(*sample.Filters); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		for _, documentKey := range sample.ExpectedDocuments {
			if _, exists := c.documents[documentKey]; !exists {
				return fieldError(path+".expected_documents", "unknown document")
			}
		}
		for _, key := range sample.ExpectedCitations {
			if _, exists := c.articles[key]; !exists {
				return fieldError(path+".expected_citations", "unknown article")
			}
		}
	}
	return nil
}
