package model

import "time"

type ToolCall struct {
	ID          string  `gorm:"type:char(36) CHARACTER SET ascii COLLATE ascii_bin;primaryKey" json:"-"`
	RunID       string  `gorm:"type:char(36) CHARACTER SET ascii COLLATE ascii_bin;not null;uniqueIndex:uk_tool_call_sequence,priority:1;uniqueIndex:uk_tool_call_model,priority:1" json:"-"`
	Sequence    uint32  `gorm:"not null;uniqueIndex:uk_tool_call_sequence,priority:2" json:"-"`
	ModelCallID *string `gorm:"type:varchar(128) CHARACTER SET ascii COLLATE ascii_bin;uniqueIndex:uk_tool_call_model,priority:2" json:"-"`
	ToolName    string  `gorm:"type:varchar(40);not null" json:"-"`

	Status         string `gorm:"type:varchar(16);not null" json:"-"`
	ErrorCode      string `gorm:"type:varchar(64);not null" json:"-"`
	BusinessEffect string `gorm:"type:varchar(16);not null" json:"-"`

	ArgumentBytes uint32 `gorm:"not null" json:"-"`
	Confirmed     bool   `gorm:"not null" json:"-"`

	OperationKey    string `gorm:"type:varchar(36) CHARACTER SET ascii COLLATE ascii_bin;not null" json:"-"`
	RequestHash     string `gorm:"type:varchar(64) CHARACTER SET ascii COLLATE ascii_bin;not null" json:"-"`
	PolicyDigest    string `gorm:"type:varchar(64) CHARACTER SET ascii COLLATE ascii_bin;not null" json:"-"`
	WriteAuthorized bool   `gorm:"not null" json:"-"`

	TicketID        *uint64            `gorm:"index" json:"-"`
	TicketStatus    AppealTicketStatus `gorm:"type:varchar(24);not null" json:"-"`
	TicketCreatedAt *time.Time         `gorm:"type:datetime(3)" json:"-"`
	Replayed        bool               `gorm:"not null" json:"-"`

	StartedAt  time.Time  `gorm:"type:datetime(3);not null;autoCreateTime:false" json:"-"`
	FinishedAt *time.Time `gorm:"type:datetime(3)" json:"-"`
	DurationMS int64      `gorm:"not null" json:"-"`

	Run AgentRun `gorm:"foreignKey:RunID;references:ID;constraint:OnDelete:RESTRICT" json:"-"`

	Ticket *AppealTicket `gorm:"foreignKey:TicketID;references:ID;constraint:OnDelete:RESTRICT" json:"-"`
}
