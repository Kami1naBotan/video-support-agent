package evaluation

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sort"
	"time"
	"video-support-agent/internal/retrieval"

	"gorm.io/gorm"
)

type RuntimeArticleID struct {
	DocumentID uint64 `json:"document_id"`
	VersionID  uint64 `json:"version_id"`
	ArticleID  uint64 `json:"article_id"`
}

type ArticleBinding struct {
	Key        CitationKey      `json:"key"`
	IDs        RuntimeArticleID `json:"ids"`
	CategoryID uint64           `json:"category_id"`
}

type CorpusBinding struct {
	corpus  *Corpus
	digest  string
	forward [4]map[string]uint64
	reverse [4]map[uint64]string
	byID    map[RuntimeArticleID]CitationKey
}

func (b *CorpusBinding) Digest() string {
	return b.digest
}

func (b *CorpusBinding) Citation(ids RuntimeArticleID) (CitationKey, bool) {
	key, exists := b.byID[ids]
	return key, exists
}

func BindCorpus(ctx context.Context, db *gorm.DB, corpus *Corpus, bindings []ArticleBinding) (*CorpusBinding, error) {
	if ctx == nil || db == nil || corpus == nil {
		return nil, fieldError("binding", "context, database and corpus are required")
	}
	if len(bindings) != len(corpus.articles) {
		return nil, fieldError("binding", "must cover every corpus article")
	}

	bound := &CorpusBinding{
		corpus: corpus,
		byID:   make(map[RuntimeArticleID]CitationKey),
	}
	for i := range bound.forward {
		bound.forward[i] = make(map[string]uint64)
		bound.reverse[i] = make(map[uint64]string)
	}
	seen := make(map[CitationKey]struct{})

	for i, binding := range bindings {
		path := fmt.Sprintf("bindings[%d]", i)
		article, exists := corpus.Article(binding.Key)
		if !exists {
			return nil, fieldError(path, "unknown corpus article")
		}
		if _, exists := seen[binding.Key]; exists {
			return nil, fieldError(path, "duplicate article binding")
		}
		seen[binding.Key] = struct{}{}

		versionKey := corpusVersionKey(binding.Key.DocumentKey, binding.Key.VersionKey)
		keys := [4]string{
			binding.Key.DocumentKey,
			versionKey,
			versionKey + "/" + binding.Key.ArticleKey,
			article.CategoryKey,
		}
		ids := [4]uint64{
			binding.IDs.DocumentID,
			binding.IDs.VersionID,
			binding.IDs.ArticleID,
			binding.CategoryID,
		}

		for kind, key := range keys {
			id := ids[kind]
			if id == 0 {
				return nil, fieldError(path, "database IDs must be nonzero")
			}
			if old, exists := bound.forward[kind][key]; exists && old != id {
				return nil, fieldError(path, "one stable key maps to multiple IDs")
			}
			if old, exists := bound.reverse[kind][id]; exists && old != key {
				return nil, fieldError(path, "one database ID maps to multiple keys")
			}
			bound.forward[kind][key] = id
			bound.reverse[kind][id] = key
		}
		bound.byID[binding.IDs] = binding.Key
	}

	for i, binding := range bindings {
		article, _ := corpus.Article(binding.Key)
		if err := verifyDatabaseArticle(ctx, db, binding, article); err != nil {
			return nil, fmt.Errorf("bindings[%d]: %w", i, err)
		}
	}

	frozen := append([]ArticleBinding(nil), bindings...)
	sort.Slice(frozen, func(i, j int) bool {
		left := frozen[i].Key
		right := frozen[j].Key
		if left.DocumentKey != right.DocumentKey {
			return left.DocumentKey < right.DocumentKey
		}
		if left.VersionKey != right.VersionKey {
			return left.VersionKey < right.VersionKey
		}
		return left.ArticleKey < right.ArticleKey
	})
	digest, err := EvidenceHash(frozen)
	if err != nil {
		return nil, err
	}
	bound.digest = digest
	return bound, nil
}

type databaseArticle struct {
	DocumentID  uint64
	VersionID   uint64
	ArticleID   uint64
	CategoryID  uint64
	Content     string
	Title       string
	SourceURL   string
	EffectiveAt *time.Time
	ExpiresAt   *time.Time
}

func verifyDatabaseArticle(ctx context.Context, db *gorm.DB, binding ArticleBinding, expected CorpusArticle) error {
	var actual databaseArticle
	err := db.WithContext(ctx).
		Table("rule_articles AS a").
		Select("a.id AS article_id, a.version_id, v.document_id, "+"v.category_id, a.content, v.title, v.source_url, "+"v.effective_at, v.expires_at").
		Joins("JOIN rule_versions v ON v.id = a.version_id").
		Joins("JOIN knowledge_documents d ON d.id = v.document_id").
		Joins("JOIN rule_categories c ON c.id = v.category_id").
		Where("a.id = ?", binding.IDs.ArticleID).
		Take(&actual).Error
	if err != nil {
		return fieldError("database", "article lookup failed")
	}

	if actual.DocumentID != binding.IDs.DocumentID || actual.VersionID != binding.IDs.VersionID || actual.ArticleID != binding.IDs.ArticleID || actual.CategoryID != binding.CategoryID {
		return fieldError("database", "identity relationship mismatch")
	}
	if !matchesCorpusContent(expected, actual.Content, actual.Title, actual.SourceURL, actual.EffectiveAt, actual.ExpiresAt) {
		return fieldError("database", "corpus content or metadata mismatch")
	}
	return nil
}

func matchesCorpusContent(expected CorpusArticle, content, title, sourceURL string, effectiveAt, expiresAt *time.Time) bool {
	if len(content) > MaxReferenceBytes || effectiveAt == nil {
		return false
	}
	if sha256.Sum256([]byte(content)) != sha256.Sum256([]byte(expected.Content)) {
		return false
	}
	if title != expected.Title || sourceURL != expected.SourceURL {
		return false
	}

	effective, err := time.Parse(time.RFC3339, expected.EffectiveAt)
	if err != nil || !effective.Equal(*effectiveAt) {
		return false
	}
	if expected.ExpiresAt == "" {
		return expiresAt == nil
	}
	expires, err := time.Parse(time.RFC3339, expected.ExpiresAt)
	return err == nil && expiresAt != nil && expires.Equal(*expiresAt)
}

func (b *CorpusBinding) SearchOptions(c Case) (retrieval.HybridOptions, error) {
	if b == nil || b.corpus == nil {
		return retrieval.HybridOptions{}, fieldError("binding", "is required")
	}
	if err := b.corpus.ValidateCases([]Case{c}); err != nil {
		return retrieval.HybridOptions{}, err
	}
	filters, err := b.corpus.ResolveFilterKeys(*c.Filters)
	if err != nil {
		return retrieval.HybridOptions{}, err
	}

	options := retrieval.HybridOptions{TopK: c.TopK}
	if filters.DocumentKey != "" {
		options.DocumentID = b.forward[0][filters.DocumentKey]
	}
	if filters.VersionKey != "" {
		options.VersionID = b.forward[1][corpusVersionKey(filters.DocumentKey, filters.VersionKey)]
	}
	if filters.CategoryKey != "" {
		options.CategoryID = b.forward[3][filters.CategoryKey]
	}
	return options, nil
}

func (b *CorpusBinding) MatchItem(item retrieval.HybridItem) (CitationKey, error) {
	chunk := item.Rule.Chunk
	ids := RuntimeArticleID{
		DocumentID: chunk.DocumentID,
		VersionID:  chunk.VersionID,
		ArticleID:  chunk.ArticleID,
	}
	key, exists := b.Citation(ids)
	if !exists {
		return CitationKey{}, fieldError("result", "unknown runtime identity")
	}
	expected, _ := b.corpus.Article(key)
	if chunk.CategoryID != b.forward[3][expected.CategoryKey] {
		return CitationKey{}, fieldError("result", "category mismatch")
	}
	if !matchesCorpusContent(expected, item.Rule.ArticleContent, chunk.Title, chunk.SourceURL, &chunk.EffectiveAt, chunk.ExpiresAt) {
		return CitationKey{}, fieldError("result", "content or metadata mismatch")
	}
	return key, nil
}
