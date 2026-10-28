package model

import "time"

type KnowledgeDocument struct {
	ID                  uint64    `gorm:"primaryKey" json:"id"`
	DocumentKey         string    `gorm:"size:100;not null;uniqueIndex:uk_document_key" json:"document_key"`
	CurrentVersionID    *uint64   `json:"current_version_id"`
	PublicationRevision uint64    `gorm:"not null;default:0" json:"publication_revision"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
}
