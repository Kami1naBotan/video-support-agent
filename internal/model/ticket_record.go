package model

import "time"

type TicketRecordAction string

const TicketRecordCreated TicketRecordAction = "created"

type TicketRecord struct {
	ID       uint64 `gorm:"primaryKey" json:"id,string"`
	TicketID uint64 `gorm:"not null;uniqueIndex:uk_ticket_record_sequence,priority:1" json:"ticket_id,string"`
	Sequence uint32 `gorm:"not null;uniqueIndex:uk_ticket_record_sequence,priority:2" json:"sequence"`

	Action      TicketRecordAction  `gorm:"type:varchar(24);not null" json:"action"`
	ActorUserID uint64              `gorm:"not null;index" json:"-"`
	FromStatus  *AppealTicketStatus `gorm:"type:varchar(24)" json:"from_status"`
	ToStatus    AppealTicketStatus  `gorm:"type:varchar(24);not null" json:"to_status"`
	ReasonCode  string              `gorm:"type:varchar(64);not null" json:"reason_code"`
	CreatedAt   time.Time           `gorm:"type:datetime(3);not null;autoCreateTime:false" json:"created_at"`

	Ticket AppealTicket `gorm:"foreignKey:TicketID;references:ID;constraint:OnDelete:RESTRICT" json:"-"`
	Actor  User         `gorm:"foreignKey:ActorUserID;references:ID;constraint:OnDelete:RESTRICT" json:"-"`
}
