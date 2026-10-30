package handler

import (
	"time"

	"video-support-agent/internal/retrieval"
)

type knowledgeSearchResponse struct {
	Items []knowledgeSearchItem `json:"items"`
	Count int                   `json:"count"`
	Limit int                   `json:"limit"`
}

type knowledgeSearchItem struct {
	ChunkID    uint64 `json:"chunk_id"`
	DocumentID uint64 `json:"document_id"`
	VersionID  uint64 `json:"version_id"`
	ArticleID  uint64 `json:"article_id"`
	CategoryID uint64 `json:"category_id"`

	ChunkIndex   uint32 `json:"chunk_index"`
	ArticleOrder uint32 `json:"article_order"`

	Version       string `json:"version"`
	Title         string `json:"title"`
	SectionPath   string `json:"section_path"`
	ArticleNumber string `json:"article_number"`
	Content       string `json:"content"`
	SourceURL     string `json:"source_url"`

	EffectiveAt time.Time  `json:"effective_at"`
	ExpiresAt   *time.Time `json:"expires_at"`

	Score      float64             `json:"score"`
	Highlights map[string][]string `json:"highlights"`
}

func toKnowledgeSearchItem(chunk retrieval.RuleChunk) knowledgeSearchItem {
	highlights := chunk.Highlights
	if highlights == nil {
		highlights = make(map[string][]string)
	}

	return knowledgeSearchItem{
		ChunkID:       chunk.ChunkID,
		DocumentID:    chunk.DocumentID,
		VersionID:     chunk.VersionID,
		ArticleID:     chunk.ArticleID,
		CategoryID:    chunk.CategoryID,
		ChunkIndex:    chunk.ChunkIndex,
		ArticleOrder:  chunk.ArticleOrder,
		Version:       chunk.Version,
		Title:         chunk.Title,
		SectionPath:   chunk.SectionPath,
		ArticleNumber: chunk.ArticleNumber,
		Content:       chunk.Content,
		SourceURL:     chunk.SourceURL,
		EffectiveAt:   chunk.EffectiveAt,
		ExpiresAt:     chunk.ExpiresAt,
		Score:         chunk.Score,
		Highlights:    highlights,
	}
}
