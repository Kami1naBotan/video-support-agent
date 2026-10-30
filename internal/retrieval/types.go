package retrieval

import (
	"context"
	"time"
)

const (
	DefaultLimit = 10
	MaxLimit     = 50

	MaxQueryRunes         = 200
	MaxSectionRunes       = 500
	MaxArticleNumberRunes = 64

	CandidateMultiplier = 5
	MaxCandidates       = MaxLimit * CandidateMultiplier
)

type Retriever interface {
	Retrieve(
		ctx context.Context,
		query string,
		opts SearchOptions,
	) ([]RuleChunk, error)
}

type SearchOptions struct {
	DocumentID uint64
	CategoryID uint64

	ArticleNumber string
	Section       string

	Limit int

	Highlight bool
}

type RuleChunk struct {
	ChunkID    uint64
	DocumentID uint64
	VersionID  uint64
	ArticleID  uint64
	CategoryID uint64

	ChunkIndex   uint32
	ArticleOrder uint32

	Version       string
	Title         string
	SectionPath   string
	ArticleNumber string
	Content       string
	SourceURL     string

	EffectiveAt time.Time
	ExpiresAt   *time.Time

	Score float64

	Highlights map[string][]string
}
