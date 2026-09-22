package model

import "time"

type Conversation struct {
	ID        uint64    `gorm:"primaryKey" json:"id"`
	UserID    uint64    `gorm:"not null;index" json:"user_id"`
	Title     string    `gorm:"size:200;not null" json:"title"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	User      User      `gorm:"foreignKey:UserID;references:ID;constraint:OnDelete:RESTRICT" json:"-"`
}
