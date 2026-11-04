package model

import "time"

type AppealSubjectType string

const (
	AppealSubjectAccount AppealSubjectType = "account"
	AppealSubjectContent AppealSubjectType = "content"
	AppealSubjectOther   AppealSubjectType = "other"
)

type AppealIntakeMode string

const (
	AppealIntakeStandard     AppealIntakeMode = "standard"
	AppealIntakeManualReview AppealIntakeMode = "manual_review"
)

type AppealTicketStatus string

const (
	AppealTicketSubmitted     AppealTicketStatus = "submitted"
	AppealTicketPendingReview AppealTicketStatus = "pending_review"
)

type AppealTicket struct {
	ID             uint64 `gorm:"primaryKey;index:idx_appeal_ticket_user_created,priority:3" json:"id,string"`
	UserID         uint64 `gorm:"not null;uniqueIndex:uk_appeal_ticket_user_key,priority:1;index:idx_appeal_ticket_user_created,priority:1" json:"-"`
	ConversationID uint64 `gorm:"not null;index" json:"-"`

	SubjectType AppealSubjectType  `gorm:"type:varchar(16);not null" json:"subject_type"`
	SubjectRef  string             `gorm:"type:varchar(128);not null" json:"subject_ref"`
	Reason      string             `gorm:"type:varchar(2000);not null" json:"-"`
	IntakeMode  AppealIntakeMode   `gorm:"type:varchar(16);not null" json:"intake_mode"`
	Status      AppealTicketStatus `gorm:"type:varchar(24);not null" json:"status"`

	IdempotencyKey string `gorm:"type:char(36) CHARACTER SET ascii COLLATE ascii_bin;not null;uniqueIndex:uk_appeal_ticket_user_key,priority:2" json:"-"`
	RequestHash    string `gorm:"type:char(64) CHARACTER SET ascii COLLATE ascii_bin;not null" json:"-"`

	EligibilitySnapshot AppealEligibilitySnapshot `gorm:"serializer:json;type:json;not null" json:"-"`

	CreatedAt time.Time `gorm:"type:datetime(3);not null;autoCreateTime:false;index:idx_appeal_ticket_user_created,priority:2" json:"created_at"`
	UpdatedAt time.Time `gorm:"type:datetime(3);not null;autoUpdateTime:false" json:"updated_at"`

	User         User         `gorm:"foreignKey:UserID;references:ID;constraint:OnDelete:RESTRICT" json:"-"`
	Conversation Conversation `gorm:"foreignKey:ConversationID;references:ID;constraint:OnDelete:RESTRICT" json:"-"`
}
