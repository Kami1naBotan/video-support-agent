package model

import "time"

type KnowledgeDocument struct {
	ID          uint64     `gorm:"primaryKey" json:"id"`
	DocumentKey string     `gorm:"size:100;not null;uniqueIndex:uk_document_version" json:"document_key"`
	Version     string     `gorm:"size:64;not null;uniqueIndex:uk_document_version" json:"version"`
	Title       string     `gorm:"size:255;not null" json:"title"`
	Category    string     `gorm:"size:64;not null;index" json:"category"`
	Content     string     `gorm:"type:longtext;not null" json:"content"`
	SourceURL   string     `gorm:"size:2048;not null" json:"source_url"`
	EffectiveAt *time.Time `json:"effective_at"`
	ExpiresAt   *time.Time `json:"expires_at"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}
