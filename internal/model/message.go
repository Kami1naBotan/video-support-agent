package model

import "time"

type Message struct {
	ID             uint64           `gorm:"primaryKey" json:"id"`
	ConversationID uint64           `gorm:"not null;index" json:"conversation_id"`
	Role           string           `gorm:"size:16;not null;uniqueIndex:uk_message_agent_run_role,priority:2" json:"role"`
	Content        string           `gorm:"type:longtext;not null" json:"content"`
	Citations      MessageCitations `gorm:"type:json" json:"citations"`
	AgentRunID     *string          `gorm:"type:char(36) CHARACTER SET ascii COLLATE ascii_bin;uniqueIndex:uk_message_agent_run_role,priority:1" json:"agent_run_id,omitempty"`
	CreatedAt      time.Time        `json:"created_at"`
	Conversation   Conversation     `gorm:"foreignKey:ConversationID;references:ID;constraint:OnDelete:RESTRICT" json:"-"`
	AgentRun       *AgentRun        `gorm:"foreignKey:AgentRunID;references:ID;constraint:OnDelete:RESTRICT" json:"-"`
}
