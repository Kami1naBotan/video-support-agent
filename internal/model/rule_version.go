package model

import "time"

type RuleVersionStatus string

const (
	RuleVersionStatusDraft     RuleVersionStatus = "draft"
	RuleVersionStatusPublished RuleVersionStatus = "published"
	RuleVersionStatusWithdrawn RuleVersionStatus = "withdrawn"
)

type RuleRevisionType string

const (
	RuleRevisionTypeUnknown   RuleRevisionType = "unknown"
	RuleRevisionTypeOfficial  RuleRevisionType = "official"
	RuleRevisionTypeEditorial RuleRevisionType = "editorial"
)

type RuleVersion struct {
	ID                uint64           `gorm:"primaryKey" json:"id"`
	DocumentID        uint64           `gorm:"not null;uniqueIndex:uk_rule_document_version,priority:1" json:"document_id"`
	Version           string           `gorm:"size:64;not null;uniqueIndex:uk_rule_document_version,priority:2" json:"version"`
	ContentRevision   uint64           `gorm:"not null;default:1" json:"content_revision"`
	CategoryID        uint64           `gorm:"not null;index" json:"category_id"`
	RevisionType      RuleRevisionType `gorm:"type:varchar(16);not null;default:unknown" json:"revision_type"`
	RevisionNote      string           `gorm:"size:2000;not null;default:''" json:"revision_note"`
	RevisionSourceURL *string          `gorm:"size:2048" json:"revision_source_url"`

	Title     string `gorm:"size:255;not null" json:"title"`
	Content   string `gorm:"type:longtext;not null" json:"content"`
	SourceURL string `gorm:"size:2048;not null" json:"source_url"`

	EffectiveAt *time.Time `json:"effective_at"`
	ExpiresAt   *time.Time `json:"expires_at"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	PublishedAt *time.Time `json:"published_at"`
	WithdrawnAt *time.Time `json:"withdrawn_at"`

	Document KnowledgeDocument `gorm:"foreignKey:DocumentID;references:ID;constraint:OnDelete:RESTRICT" json:"-"`
	Category RuleCategory      `gorm:"foreignKey:CategoryID;references:ID;constraint:OnDelete:RESTRICT" json:"-"`
	Status   RuleVersionStatus `gorm:"type:varchar(20);not null;default:draft" json:"status"`
}
