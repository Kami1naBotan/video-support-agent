//go:build workerkafka

package workeracceptance

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"video-support-agent/internal/model"
)

// The fixture verifies the disposable MySQL server identity before any DDL.
func TestWorkerObservationMigrationKafka(t *testing.T) {
	db, _ := workerSQLFixture(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	cases := []struct {
		name string
		row  any
	}{
		{"event_outboxes", &model.EventOutbox{MessageID: "legacy-message", DedupKey: "legacy-dedup", EventID: "legacy-event", Payload: []byte("legacy-payload"), PayloadSHA256: "legacy-hash", Status: "pending", NextAttemptAt: now}},
		{"knowledge_event_inboxes", &model.KnowledgeEventInbox{EventID: "legacy-event", Payload: []byte("legacy-payload"), PayloadSHA256: "legacy-hash", Status: "received", ReceivedAt: now}},
		{"knowledge_index_attempts", &model.KnowledgeIndexAttempt{EventID: "legacy-event", Attempt: 1, SourceSHA256: "legacy-hash", Status: "scheduled", NotBefore: now}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, db.Migrator().DropColumn(tc.row, "TraceParent"))
			require.NoError(t, db.Omit("TraceParent").Create(tc.row).Error)
			var before []map[string]any
			require.NoError(t, db.Table(tc.name).Find(&before).Error)
			require.Len(t, before, 1)
			require.NoError(t, db.AutoMigrate(tc.row))
			require.NoError(t, db.AutoMigrate(tc.row))
			var after []map[string]any
			require.NoError(t, db.Table(tc.name).Find(&after).Error)
			require.Len(t, after, 1)
			parent, exists := after[0]["trace_parent"]
			require.True(t, exists)
			require.NotNil(t, parent)
			require.Empty(t, parent)
			delete(after[0], "trace_parent")
			require.Equal(t, before, after, "migration must preserve all historical fields")
			var nullable string
			require.NoError(t, db.Raw("SELECT IS_NULLABLE FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME=? AND COLUMN_NAME='trace_parent'", tc.name).Scan(&nullable).Error)
			require.Equal(t, "NO", nullable)
		})
	}
}
