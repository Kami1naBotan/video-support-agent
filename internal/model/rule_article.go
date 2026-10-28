package model

import "time"

type RuleArticle struct {
	ID           uint64 `gorm:"primaryKey" json:"id"`
	VersionID    uint64 `gorm:"not null;uniqueIndex:uk_rule_version_article_order,priority:1" json:"version_id"`
	ArticleOrder uint32 `gorm:"not null;uniqueIndex:uk_rule_version_article_order,priority:2" json:"article_order"`

	SectionPath   string `gorm:"size:500;not null" json:"section_path"`
	ArticleNumber string `gorm:"size:64;not null" json:"article_number"`
	Content       string `gorm:"type:longtext;not null" json:"content"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	Version RuleVersion `gorm:"foreignKey:VersionID;references:ID;constraint:OnDelete:RESTRICT" json:"-"`
}
