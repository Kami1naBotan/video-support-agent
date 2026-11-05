package model

import "time"

const (
	KnowledgeRecoveryPending   = "pending"
	KnowledgeRecoveryCompleted = "completed"
)

type KnowledgeBuildRecovery struct {
	GenerationID string `gorm:"type:char(36);primaryKey;->;<-:create"`
	SourceEpoch  uint64 `gorm:"not null;index;->;<-:create"`

	FailedAttempt  uint32 `gorm:"not null;->;<-:create"`
	FailureCode    string `gorm:"size:64;not null;->;<-:create"`
	ReasonCode     string `gorm:"size:64;not null;->;<-:create"`
	Action         string `gorm:"size:24;not null;->;<-:create"`
	NextAttempt    uint32 `gorm:"not null;->;<-:create"`
	JitterPermille uint32 `gorm:"not null;->;<-:create"`

	DecidedAt time.Time  `gorm:"type:datetime(3);not null;->;<-:create"`
	NotBefore *time.Time `gorm:"type:datetime(3);->;<-:create"`

	Status      string     `gorm:"size:24;not null;index"`
	CompletedAt *time.Time `gorm:"type:datetime(3)"`
	CreatedAt   time.Time  `gorm:"type:datetime(3);not null"`
	UpdatedAt   time.Time  `gorm:"type:datetime(3);not null"`
}

type KnowledgeBuildEvent struct {
	GenerationID string `gorm:"type:char(36);primaryKey;->;<-:create"`
	EventID      string `gorm:"type:char(36);primaryKey;->;<-:create"`

	Attempt      uint32 `gorm:"not null;->;<-:create"`
	SourceSHA256 string `gorm:"type:char(64);not null;->;<-:create"`

	RecoveryProcessedAt *time.Time `gorm:"type:datetime(3);index"`
	CreatedAt           time.Time  `gorm:"type:datetime(3);not null"`
}

type KnowledgeRetryReceipt struct {
	MessageID string `gorm:"type:char(36);primaryKey;->;<-:create"`

	EventID string `gorm:"type:char(36);not null;uniqueIndex:uk_knowledge_retry_event_attempt,priority:1;->;<-:create"`
	Attempt uint32 `gorm:"not null;uniqueIndex:uk_knowledge_retry_event_attempt,priority:2;->;<-:create"`

	PayloadSHA256 string `gorm:"type:char(64);not null;->;<-:create"`
	SourceSHA256  string `gorm:"type:char(64);not null;->;<-:create"`

	Topic     string `gorm:"size:128;not null;->;<-:create"`
	Partition int32  `gorm:"not null;->;<-:create"`
	Offset    int64  `gorm:"not null;->;<-:create"`

	ReceivedAt time.Time `gorm:"type:datetime(3);not null;->;<-:create"`
}
