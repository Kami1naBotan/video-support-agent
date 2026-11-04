package model

import "time"

type AgentRun struct {
	ID                string     `gorm:"type:char(36) CHARACTER SET ascii COLLATE ascii_bin;primaryKey" json:"-"`
	UserID            uint64     `gorm:"not null;index:idx_agent_run_owner,priority:1" json:"-"`
	ConversationID    uint64     `gorm:"not null;index:idx_agent_run_owner,priority:2" json:"-"`
	ExecutionStatus   string     `gorm:"type:varchar(16);not null" json:"-"`
	Decision          string     `gorm:"type:varchar(32);not null" json:"-"`
	ReasonCode        string     `gorm:"type:varchar(64);not null" json:"-"`
	BusinessEffect    string     `gorm:"type:varchar(16);not null" json:"-"`
	MessageSaveStatus string     `gorm:"type:varchar(16);not null" json:"-"`
	ModelCalls        uint32     `gorm:"not null" json:"-"`
	ToolCalls         uint32     `gorm:"not null" json:"-"`
	StartedAt         time.Time  `gorm:"type:datetime(3);not null;autoCreateTime:false" json:"-"`
	FinishedAt        *time.Time `gorm:"type:datetime(3)" json:"-"`

	User User `gorm:"foreignKey:UserID;references:ID;constraint:OnDelete:RESTRICT" json:"-"`

	Conversation Conversation `gorm:"foreignKey:ConversationID;references:ID;constraint:OnDelete:RESTRICT" json:"-"`
}
