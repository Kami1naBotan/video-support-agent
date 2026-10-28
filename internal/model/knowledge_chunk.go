package model

import "time"

type KnowledgeChunk struct {
	ID         uint64 `gorm:"primaryKey" json:"id"`
	ArticleID  uint64 `gorm:"not null;uniqueIndex:uk_article_chunk_index,priority:1" json:"article_id"`
	ChunkIndex uint32 `gorm:"not null;uniqueIndex:uk_article_chunk_index,priority:2" json:"chunk_index"`
	Content    string `gorm:"type:longtext;not null" json:"content"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	Article RuleArticle `gorm:"foreignKey:ArticleID;references:ID;constraint:OnDelete:RESTRICT" json:"-"`
}
