package model

import "time"

type RuleCitation struct {
	RefID string `json:"ref_id"`

	DocumentID string   `json:"document_id"`
	VersionID  string   `json:"version_id"`
	ArticleID  string   `json:"article_id"`
	CategoryID string   `json:"category_id"`
	ChunkID    string   `json:"chunk_id"`
	ChunkIDs   []string `json:"chunk_ids"`

	Version       string `json:"version"`
	Title         string `json:"title"`
	SectionPath   string `json:"section_path"`
	ArticleNumber string `json:"article_number"`
	SourceURL     string `json:"source_url"`

	EffectiveAt time.Time  `json:"effective_at"`
	ExpiresAt   *time.Time `json:"expires_at"`
	RetrievedAt time.Time  `json:"retrieved_at"`
}

func (c RuleCitation) Clone() RuleCitation {
	c.ChunkIDs = append([]string(nil), c.ChunkIDs...)

	if c.ExpiresAt != nil {
		value := *c.ExpiresAt
		c.ExpiresAt = &value
	}

	return c
}
