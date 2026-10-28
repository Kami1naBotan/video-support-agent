package model

import "time"

type RuleOperationType string

const (
	RuleOperationTypePublish  RuleOperationType = "publish"
	RuleOperationTypeWithdraw RuleOperationType = "withdraw"
)

type RuleOperation struct {
	ID uint64 `gorm:"primaryKey" json:"id"`

	DocumentID uint64 `gorm:"not null;uniqueIndex:uk_rule_operation_document_revision,priority:1" json:"document_id"`
	VersionID  uint64 `gorm:"not null;index" json:"version_id"`

	OperationType RuleOperationType `gorm:"type:varchar(16);not null" json:"operation_type"`

	BeforeCurrentVersionID *uint64 `gorm:"index" json:"before_current_version_id"`
	AfterCurrentVersionID  *uint64 `gorm:"index" json:"after_current_version_id"`

	BeforePublicationRevision uint64 `gorm:"not null" json:"before_publication_revision"`
	AfterPublicationRevision  uint64 `gorm:"not null;uniqueIndex:uk_rule_operation_document_revision,priority:2" json:"after_publication_revision"`

	OperatorID uint64    `gorm:"not null;index" json:"operator_id"`
	OperatedAt time.Time `gorm:"type:datetime(3);not null" json:"operated_at"`

	Document KnowledgeDocument `gorm:"foreignKey:DocumentID;references:ID;constraint:OnDelete:RESTRICT" json:"-"`
	Version  RuleVersion       `gorm:"foreignKey:VersionID;references:ID;constraint:OnDelete:RESTRICT" json:"-"`

	BeforeCurrentVersion *RuleVersion `gorm:"foreignKey:BeforeCurrentVersionID;references:ID;constraint:OnDelete:RESTRICT" json:"-"`
	AfterCurrentVersion  *RuleVersion `gorm:"foreignKey:AfterCurrentVersionID;references:ID;constraint:OnDelete:RESTRICT" json:"-"`

	Operator User `gorm:"foreignKey:OperatorID;references:ID;constraint:OnDelete:RESTRICT" json:"-"`
}
