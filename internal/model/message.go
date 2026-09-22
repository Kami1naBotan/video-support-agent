package model

import "time"

type Message struct {
	ID             uint64       `gorm:"primaryKey" json:"id"`
	ConversationID uint64       `gorm:"not null;index" json:"conversation_id"`
	Role           string       `gorm:"size:16;not null" json:"role"`
	Content        string       `gorm:"type:longtext;not null" json:"content"`
	CreatedAt      time.Time    `json:"created_at"`
	Conversation   Conversation `gorm:"foreignKey:ConversationID;references:ID;constraint:OnDelete:RESTRICT" json:"-"`
}
