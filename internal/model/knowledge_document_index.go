package model

import "time"

const (
	KnowledgeDocumentIndexPending = "pending"
	KnowledgeDocumentIndexReady   = "ready"
	KnowledgeDocumentIndexFailed  = "failed"
)

type KnowledgeDocumentIndex struct {
	DocumentID uint64 `gorm:"primaryKey;autoIncrement:false"`

	ExpectedPublicationRevision uint64  `gorm:"not null"`
	AppliedPublicationRevision  uint64  `gorm:"not null;default:0"`
	ExpectedEpoch               uint64  `gorm:"not null;index"`
	AppliedEpoch                uint64  `gorm:"not null;default:0"`
	AppliedGenerationID         *string `gorm:"type:char(36);index"`

	Status        string     `gorm:"size:24;not null;index"`
	SafeErrorCode string     `gorm:"size:64;not null;default:''"`
	AppliedAt     *time.Time `gorm:"type:datetime(3)"`
	CreatedAt     time.Time  `gorm:"type:datetime(3);not null"`
	UpdatedAt     time.Time  `gorm:"type:datetime(3);not null"`
}
