package model

import "time"

const (
	KnowledgeTaskPending   = "pending"
	KnowledgeTaskBuilding  = "building"
	KnowledgeTaskRetryWait = "retry_wait"
	KnowledgeTaskCompleted = "completed"
	KnowledgeTaskObsolete  = "obsolete"
	KnowledgeTaskBlocked   = "blocked"
)

type KnowledgeIndexTask struct {
	SourceEpoch uint64 `gorm:"primaryKey;autoIncrement:false;->;<-:create"`

	Status       string    `gorm:"size:24;not null;index:idx_knowledge_task_due,priority:1"`
	NotBefore    time.Time `gorm:"type:datetime(3);not null;index:idx_knowledge_task_due,priority:2"`
	GenerationID *string   `gorm:"type:char(36);index"`

	FailedAttempts uint32     `gorm:"not null;default:0"`
	SafeErrorCode  string     `gorm:"size:64;not null;default:''"`
	CompletedAt    *time.Time `gorm:"type:datetime(3)"`
	CreatedAt      time.Time  `gorm:"type:datetime(3);not null"`
	UpdatedAt      time.Time  `gorm:"type:datetime(3);not null"`
}
