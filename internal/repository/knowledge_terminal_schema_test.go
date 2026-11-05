package repository

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"video-support-agent/internal/model"
)

// Exercises the real settlement query in DryRun mode: no database or migration.
// Each literal table reference must match the model name used by migrations.
func TestSettlementQueryUsesModelTables(t *testing.T) {
	db, err := gorm.Open(mysql.New(mysql.Config{
		DSN:                       "unused:unused@tcp(127.0.0.1:1)/unused",
		SkipInitializeWithVersion: true,
	}), &gorm.Config{DryRun: true, DisableAutomaticPing: true})
	if err != nil {
		t.Fatal(err)
	}
	sql := db.ToSQL(func(tx *gorm.DB) *gorm.DB {
		if _, err := settleReadyEvents(tx, model.KnowledgeIndexControl{}, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		return tx
	})
	if sql == "" {
		t.Fatal("settlement produced no SQL")
	}
	for _, pattern := range []string{
		`JOIN\s+knowledge_index_attempts\s+AS\s+a\s+ON\s+a\.event_id\s*=\s*i\.event_id`,
		`LEFT\s+JOIN\s+knowledge_document_indices\s+AS\s+s\s+ON\s+s\.document_id\s*=\s*i\.document_id`,
	} {
		if !regexp.MustCompile(pattern).MatchString(sql) {
			t.Errorf("settlement JOIN alias/ON boundary invalid; pattern=%q; SQL: %s", pattern, sql)
		}
	}
	for _, value := range []any{
		&model.KnowledgeEventInbox{}, &model.KnowledgeDocument{},
		&model.KnowledgeIndexAttempt{}, &model.KnowledgeDocumentIndex{},
		&model.KnowledgeBuildEvent{}, &model.KnowledgeBuildRecovery{},
	} {
		stmt := &gorm.Statement{DB: db}
		if err := stmt.Parse(value); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(sql, stmt.Schema.Table+" AS ") {
			t.Errorf("settlement query does not reference model table %q; SQL: %s", stmt.Schema.Table, sql)
		}
	}
}
