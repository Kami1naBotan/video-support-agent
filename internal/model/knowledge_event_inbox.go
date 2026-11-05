package model

import "time"

const (
	KnowledgeEventReceived     = "received"
	KnowledgeEventWaiting      = "waiting"
	KnowledgeEventApplied      = "applied"
	KnowledgeEventIgnoredDraft = "ignored_draft"
	KnowledgeEventSuperseded   = "superseded"
	KnowledgeEventDeadLettered = "dead_lettered"
)

const (
	KnowledgeAttemptScheduled = "scheduled"
	KnowledgeAttemptRunning   = "running"
	KnowledgeAttemptSucceeded = "succeeded"
	KnowledgeAttemptFailed    = "failed"
	KnowledgeAttemptObsolete  = "obsolete"
)

const (
	KnowledgeDecisionIgnoreDraft = "ignore_draft"
	KnowledgeDecisionSupersede   = "supersede"
	KnowledgeDecisionRefresh     = "refresh"
)

type KnowledgeEventInbox struct {
	EventID string `gorm:"type:char(36);primaryKey;->;<-:create"`

	EventType           string `gorm:"size:64;not null;->;<-:create"`
	DocumentID          uint64 `gorm:"not null;index;->;<-:create"`
	VersionID           uint64 `gorm:"not null;->;<-:create"`
	PublicationRevision uint64 `gorm:"not null;->;<-:create"`
	TraceID             string `gorm:"type:char(32);not null;->;<-:create"`
	Payload             []byte `gorm:"type:mediumblob;not null;->;<-:create"`
	PayloadSHA256       string `gorm:"type:char(64);not null;->;<-:create"`

	TargetEpoch     *uint64 `gorm:"index"`
	ExpectedAttempt uint32  `gorm:"not null;default:1"`
	Status          string  `gorm:"size:24;not null;index"`
	Decision        string  `gorm:"size:24;not null;default:''"`

	TerminalMessageID *string    `gorm:"type:char(36);uniqueIndex:uk_knowledge_event_terminal"`
	SafeErrorCode     string     `gorm:"size:64;not null;default:''"`
	ReceivedAt        time.Time  `gorm:"type:datetime(3);not null"`
	CompletedAt       *time.Time `gorm:"type:datetime(3)"`
	CreatedAt         time.Time  `gorm:"type:datetime(3);not null"`
	UpdatedAt         time.Time  `gorm:"type:datetime(3);not null"`
}

type KnowledgeIndexAttempt struct {
	EventID string `gorm:"type:char(36);primaryKey;->;<-:create"`
	Attempt uint32 `gorm:"primaryKey;autoIncrement:false;->;<-:create"`

	SourceSHA256 string `gorm:"type:char(64);not null;->;<-:create"`
	Topic        string `gorm:"size:128;not null;->;<-:create"`
	Partition    int32  `gorm:"not null;->;<-:create"`
	Offset       int64  `gorm:"not null;->;<-:create"`

	TargetEpoch  *uint64    `gorm:"index"`
	GenerationID *string    `gorm:"type:char(36);index"`
	Status       string     `gorm:"size:24;not null;index:idx_knowledge_attempt_due,priority:1"`
	NotBefore    time.Time  `gorm:"type:datetime(3);not null;index:idx_knowledge_attempt_due,priority:2"`
	StartedAt    *time.Time `gorm:"type:datetime(3)"`
	CompletedAt  *time.Time `gorm:"type:datetime(3)"`

	SafeErrorCode string    `gorm:"size:64;not null;default:''"`
	CreatedAt     time.Time `gorm:"type:datetime(3);not null"`
	UpdatedAt     time.Time `gorm:"type:datetime(3);not null"`
}
