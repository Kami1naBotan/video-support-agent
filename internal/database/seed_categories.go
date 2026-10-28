package database

import (
	"context"
	"fmt"

	"video-support-agent/internal/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func SeedRuleCategories(ctx context.Context, db *gorm.DB) error {
	categories := []model.RuleCategory{
		{Code: "account", Name: "账号管理"},
		{Code: "submission", Name: "投稿审核"},
		{Code: "copyright", Name: "版权申诉"},
		{Code: "community", Name: "社区规范"},
		{Code: "danmaku", Name: "弹幕规范"},
	}

	err := db.WithContext(ctx).
		Clauses(clause.OnConflict{DoNothing: true}).
		Create(&categories).Error
	if err != nil {
		return fmt.Errorf("seed rule categories: %w", err)
	}

	return nil
}
