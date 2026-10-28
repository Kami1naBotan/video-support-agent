package model

import "time"

type RuleVerification struct {
	ID                      uint64      `gorm:"primaryKey" json:"id"`
	VersionID               uint64      `gorm:"not null;index:idx_verification_version_revision,priority:1" json:"version_id"`
	VerifiedContentRevision uint64      `gorm:"not null;index:idx_verification_version_revision,priority:2" json:"verified_content_revision"`
	VerifiedBy              uint64      `gorm:"not null;index" json:"verified_by"`
	VerifiedAt              time.Time   `gorm:"not null" json:"verified_at"`
	VerificationNote        string      `gorm:"size:2000;not null" json:"verification_note"`
	EvidenceURL             *string     `gorm:"size:2048" json:"evidence_url"`
	SnapshotSchemaVersion   uint32      `gorm:"not null;default:1" json:"snapshot_schema_version"`
	ContentSnapshot         string      `gorm:"type:json;not null" json:"-"`
	Version                 RuleVersion `gorm:"foreignKey:VersionID;references:ID;constraint:OnDelete:RESTRICT" json:"-"`
	Verifier                User        `gorm:"foreignKey:VerifiedBy;references:ID;constraint:OnDelete:RESTRICT" json:"-"`
}
