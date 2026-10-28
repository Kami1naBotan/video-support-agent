package router

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"video-support-agent/internal/config"
)

// TestKnowledgeAcceptanceReadinessMySQL is read-only.
// It does not migrate, seed, delete or update any database records.
func TestKnowledgeAcceptanceReadinessMySQL(t *testing.T) {
	if os.Getenv("RUN_MYSQL_TESTS") != "1" {
		t.Fatal("require RUN_MYSQL_TESTS=1")
	}
	if os.Getenv("DB_NAME") != "bili_support_test" {
		t.Fatal("require explicit DB_NAME=bili_support_test")
	}
	if os.Getenv("TEST_ISOLATED") != "1" {
		t.Fatal("stop other writers, then explicitly set TEST_ISOLATED=1")
	}

	expectedUUID := strings.TrimSpace(os.Getenv("TEST_MYSQL_SERVER_UUID"))
	if expectedUUID == "" {
		t.Fatal("confirm the target instance, then set TEST_MYSQL_SERVER_UUID")
	}

	cfg := config.Load()

	// Reuse existing read-only database/schema/isolation checks.
	// OpenTestDB registers connection-pool cleanup with t.Cleanup.
	db := OpenTestDB(t, cfg.Database)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var identity struct {
		DatabaseName string
		ServerUUID   string
		Account      string
	}

	result := db.WithContext(ctx).Raw(`
		SELECT
			DATABASE() AS database_name,
			@@server_uuid AS server_uuid,
			CURRENT_USER() AS account
	`).Scan(&identity)
	if result.Error != nil {
		t.Fatalf("read database identity: %v", result.Error)
	}
	if result.RowsAffected != 1 {
		t.Fatalf("expected one identity row, got %d", result.RowsAffected)
	}
	if identity.DatabaseName != "bili_support_test" {
		t.Fatalf("unexpected database: %q", identity.DatabaseName)
	}
	if identity.ServerUUID != expectedUUID {
		t.Fatalf(
			"server UUID mismatch: expected=%q actual=%q",
			expectedUUID,
			identity.ServerUUID,
		)
	}
	if identity.Account == "" {
		t.Fatal("database account identity is empty")
	}

	t.Logf(
		"confirmed database=%s server_uuid=%s account=%s",
		identity.DatabaseName,
		identity.ServerUUID,
		identity.Account,
	)

	var categoryCount int64
	if err := db.WithContext(ctx).
		Table("rule_categories").
		Count(&categoryCount).Error; err != nil {
		t.Fatalf("count rule categories: %v", err)
	}

	t.Logf("rule_categories initial rows=%d", categoryCount)

	if categoryCount != 0 {
		t.Fatalf(
			"full-suite prerequisite not met: "+
				"TestSeedRuleCategoriesMySQL requires an initially empty "+
				"rule_categories table; found %d rows; "+
				"preserve existing data and arrange an isolated test environment",
			categoryCount,
		)
	}

	t.Log("read-only readiness passed; no INSERT/UPDATE/DELETE/DDL executed")
	t.Log("TEST_ISOLATED is an operator declaration, not proof that no other writer exists")
}
