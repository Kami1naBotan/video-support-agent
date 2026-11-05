package model

import "time"

const KnowledgeIndexControlID uint64 = 1

type KnowledgeIndexControl struct {
	ID                 uint64     `gorm:"primaryKey;autoIncrement:false"`
	DesiredEpoch       uint64     `gorm:"not null"`
	ActiveEpoch        uint64     `gorm:"not null"`
	ActiveGenerationID *string    `gorm:"type:char(36)"`
	GenerationLimit    uint64     `gorm:"not null;default:0"`
	OwnerToken         *string    `gorm:"type:char(36)"`
	LeaseUntil         *time.Time `gorm:"type:datetime(3)"`
	CreatedAt          time.Time  `gorm:"type:datetime(3);not null"`
	UpdatedAt          time.Time  `gorm:"type:datetime(3);not null"`
}

type KnowledgeIndexBootstrap struct {
	ID          uint64     `gorm:"primaryKey;autoIncrement:false"`
	Status      string     `gorm:"size:16;not null"`
	CreatedAt   time.Time  `gorm:"type:datetime(3);not null"`
	CompletedAt *time.Time `gorm:"type:datetime(3)"`
}
