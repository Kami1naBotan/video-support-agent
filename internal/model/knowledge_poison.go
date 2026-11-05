package model

import "time"

type KnowledgePoison struct {
	PositionID string `gorm:"type:char(64);primaryKey;->;<-:create"`

	Topic     string `gorm:"size:128;not null;uniqueIndex:uk_knowledge_poison_location,priority:1;->;<-:create"`
	Partition int32  `gorm:"not null;uniqueIndex:uk_knowledge_poison_location,priority:2;->;<-:create"`
	Offset    int64  `gorm:"not null;uniqueIndex:uk_knowledge_poison_location,priority:3;->;<-:create"`

	RecordSHA256 string `gorm:"type:char(64);not null;->;<-:create"`
	ReasonCode   string `gorm:"size:64;not null;->;<-:create"`

	Notice []byte `gorm:"type:blob;not null;->;<-:create"`

	CreatedAt time.Time `gorm:"type:datetime(3);not null;->;<-:create"`
}
