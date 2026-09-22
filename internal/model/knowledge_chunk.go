package model

import "time"

type KnowledgeChunk struct {
	ID            uint64            `gorm:"primaryKey" json:"id"`
	DocumentID    uint64            `gorm:"not null;uniqueIndex:uk_document_index" json:"document_id"`
	ChunkIndex    int               `gorm:"not null;uniqueIndex:uk_document_index" json:"chunk_index"`
	Section       string            `gorm:"size:255;not null" json:"section"`
	ArticleNumber string            `gorm:"size:64;not null" json:"article_number"`
	Content       string            `gorm:"type:longtext;not null" json:"content"`
	CreatedAt     time.Time         `json:"created_at"`
	Document      KnowledgeDocument `gorm:"foreignKey:DocumentID;references:ID;constraint:OnDelete:RESTRICT" json:"-"`
}
