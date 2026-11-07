package model

import "time"

const (
	OutboxPending = "pending"
	OutboxSending = "sending"
	OutboxSent    = "sent"
	OutboxBlocked = "blocked"
)

type EventOutbox struct {
	ID            uint64 `gorm:"primaryKey;index:idx_outbox_document,priority:3"`
	MessageID     string `gorm:"type:char(36);not null;uniqueIndex:uk_outbox_message;->;<-:create"`
	DedupKey      string `gorm:"size:128;not null;uniqueIndex:uk_outbox_dedup;->;<-:create"`
	EventID       string `gorm:"type:char(36);not null;index;->;<-:create"`
	Topic         string `gorm:"size:128;not null;index:idx_outbox_document,priority:1;->;<-:create"`
	MessageKey    string `gorm:"size:80;not null;index:idx_outbox_document,priority:2;->;<-:create"`
	TraceID       string `gorm:"type:char(32);not null;->;<-:create"`
	TraceParent   string `gorm:"size:55;not null;default:'';->;<-:create"`
	Payload       []byte `gorm:"type:mediumblob;not null;->;<-:create"`
	PayloadSHA256 string `gorm:"type:char(64);not null;->;<-:create"`

	Status        string     `gorm:"size:16;not null;index:idx_outbox_ready,priority:1;index:idx_outbox_lease,priority:1"`
	Attempts      uint64     `gorm:"not null;default:0"`
	NextAttemptAt time.Time  `gorm:"type:datetime(3);not null;index:idx_outbox_ready,priority:2"`
	OwnerToken    *string    `gorm:"type:char(36)"`
	LeaseUntil    *time.Time `gorm:"type:datetime(3);index:idx_outbox_lease,priority:2"`
	SafeErrorCode string     `gorm:"size:64;not null;default:''"`
	SentAt        *time.Time `gorm:"type:datetime(3)"`
	CreatedAt     time.Time  `gorm:"type:datetime(3);not null"`
	UpdatedAt     time.Time  `gorm:"type:datetime(3);not null"`
}
