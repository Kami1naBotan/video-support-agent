package database_test

import (
	"context"
	"os"
	"testing"
	"time"

	"video-support-agent/internal/config"
	"video-support-agent/internal/database"
	"video-support-agent/internal/model"
)

func TestSeedRuleCategoriesMySQL(t *testing.T) {
	if os.Getenv("RUN_MYSQL_TESTS") != "1" {
		t.Skip("set RUN_MYSQL_TESTS=1 to run MySQL integration tests")
	}

	cfg := config.Load()
	if cfg.Database.Name != "bili_support_test" {
		t.Fatal("this test requires DB_NAME=bili_support_test")
	}

	db, err := database.OpenMySQL(cfg.Database)
	if err != nil {
		t.Fatal(err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = sqlDB.Close()
	})

	// 仅准备测试库的分类表；DDL 不属于后面的事务回滚范围。
	if err := db.AutoMigrate(&model.RuleCategory{}); err != nil {
		t.Fatalf("prepare category table: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	tx := db.WithContext(ctx).Begin()
	if tx.Error != nil {
		t.Fatalf("begin transaction: %v", tx.Error)
	}
	defer func() {
		if err := tx.Rollback().Error; err != nil {
			t.Errorf("rollback test transaction: %v", err)
		}
	}()

	// 不删除已有测试数据。若不为空，先人工确认测试环境。
	var initialCount int64
	if err := tx.Model(&model.RuleCategory{}).
		Count(&initialCount).Error; err != nil {
		t.Fatal(err)
	}
	if initialCount != 0 {
		t.Fatalf(
			"test requires an empty rule_categories table; found %d rows",
			initialCount,
		)
	}

	// 模拟一个已经存在、名称与预置名称不同的分类。
	existing := model.RuleCategory{
		Code: "account",
		Name: "已有账号分类",
	}
	if err := tx.Create(&existing).Error; err != nil {
		t.Fatalf("create existing category: %v", err)
	}

	wantNames := map[string]string{
		"account":    "已有账号分类",
		"submission": "投稿审核",
		"copyright":  "版权申诉",
		"community":  "社区规范",
		"danmaku":    "弹幕规范",
	}

	// 第一次补齐缺失分类；第二次验证重复执行的效果。
	for round := 1; round <= 2; round++ {
		if err := database.SeedRuleCategories(ctx, tx); err != nil {
			t.Fatalf("seed round %d: %v", round, err)
		}

		var categories []model.RuleCategory
		if err := tx.Order("code ASC").Find(&categories).Error; err != nil {
			t.Fatalf("read categories: %v", err)
		}

		if len(categories) != len(wantNames) {
			t.Fatalf(
				"round %d: expected %d categories, got %d",
				round, len(wantNames), len(categories),
			)
		}

		seen := make(map[string]bool)
		for _, category := range categories {
			wantName, ok := wantNames[category.Code]
			if !ok {
				t.Fatalf("unexpected category code %q", category.Code)
			}
			if seen[category.Code] {
				t.Fatalf("duplicate category code %q", category.Code)
			}
			seen[category.Code] = true

			if category.Name != wantName {
				t.Errorf(
					"round %d: category %q: expected name %q, got %q",
					round, category.Code, wantName, category.Name,
				)
			}
			if category.Code == "account" && category.ID != existing.ID {
				t.Errorf(
					"existing category ID changed: want %d, got %d",
					existing.ID, category.ID,
				)
			}
		}
	}
}
