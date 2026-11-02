//go:build ragstageintegration

package repository_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"video-support-agent/internal/config"
	"video-support-agent/internal/database"
	"video-support-agent/internal/model"
	"video-support-agent/internal/repository"

	driver "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
)

const stageMySQLUUID = "9ae2c7f4-d375-11f1-8945-ce027cf8cce1"

func stageMust(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		return
	}

	text := err.Error()
	for _, key := range []string{"RAG_STAGE_DB_PASSWORD", "LLM_API_KEY"} {
		if secret := os.Getenv(key); secret != "" {
			text = strings.ReplaceAll(text, secret, "<redacted>")
		}
	}
	t.Fatal(text)
}

func stageRequire(t *testing.T, ok bool, message string) {
	t.Helper()
	if !ok {
		t.Fatal(message)
	}
}

func stageRandom(t *testing.T) string {
	t.Helper()
	var value [16]byte
	_, err := rand.Read(value[:])
	stageMust(t, err)
	return hex.EncodeToString(value[:])
}

func stageWrite(name string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(os.Getenv("RAG_STAGE_EVIDENCE"), name)
	if err := os.WriteFile(path+".tmp", data, 0600); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

func stageSave(t *testing.T, name string, value any) {
	t.Helper()
	stageMust(t, stageWrite(name, value))
}

func stageDB(t *testing.T) *gorm.DB {
	t.Helper()
	stageRequire(t,
		os.Getenv("RAG_STAGE_RUN") == "1" &&
			os.Getenv("RAG_STAGE_DB_PASSWORD") != "" &&
			filepath.IsAbs(os.Getenv("RAG_STAGE_EVIDENCE")),
		"explicit isolated stage environment is required",
	)

	cfg := config.DatabaseConfig{
		Host:     "127.0.0.1",
		Port:     "13308",
		User:     "root",
		Password: os.Getenv("RAG_STAGE_DB_PASSWORD"),
	}

	admin, err := database.OpenMySQL(cfg)
	stageMust(t, err)
	adminPool, err := admin.DB()
	stageMust(t, err)
	t.Cleanup(func() { _ = adminPool.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var uuid, version string
	stageMust(t, adminPool.QueryRowContext(ctx,
		"SELECT @@server_uuid, VERSION()",
	).Scan(&uuid, &version))

	stageRequire(t,
		uuid == stageMySQLUUID && strings.HasPrefix(version, "8.4."),
		"isolated MySQL identity differs; no database was created",
	)

	name := "rag_stage_it_" + stageRandom(t)
	record := map[string]any{
		"database":           name,
		"server_uuid":        uuid,
		"mysql_version":      version,
		"fixture_kind":       "synthetic_not_official",
		"resources_retained": true,
	}
	stageSave(t, t.Name()+"-database-planned.json", record)

	_, err = adminPool.ExecContext(ctx,
		"CREATE DATABASE `"+name+
			"` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci")
	stageMust(t, err)

	cfg.Name = name
	db, err := database.OpenMySQL(cfg)
	stageMust(t, err)
	pool, err := db.DB()
	stageMust(t, err)
	t.Cleanup(func() { _ = pool.Close() })

	stageSave(t, t.Name()+"-database-created.json", record)
	t.Logf("ISOLATED_DATABASE=%s RESOURCES_RETAINED=true", name)

	// 保留建表选项，并让后续每次操作使用独立的 Statement。
	return db.Set("gorm:table_options", "ENGINE=InnoDB").Session(&gorm.Session{})
}

func stageCount(t *testing.T, db *gorm.DB) int64 {
	t.Helper()
	var count int64
	stageMust(t, db.Model(&model.Message{}).Count(&count).Error)
	return count
}

func stageCitations() model.MessageCitations {
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	expires := at.Add(24 * time.Hour)
	return model.MessageCitations{{
		RefID:         "R1",
		DocumentID:    "9007199254740993",
		VersionID:     "9007199254740995",
		ArticleID:     "9007199254740997",
		CategoryID:    "18446744073709551615",
		ChunkID:       "101",
		ChunkIDs:      []string{"101", "102"},
		Version:       "synthetic-v1",
		Title:         "【虚构测试】事务引用",
		SectionPath:   "测试章节",
		ArticleNumber: "TEST-1",
		SourceURL:     "https://www.bilibili.com/__rag_stage_fixture__",
		EffectiveAt:   at,
		ExpiresAt:     &expires,
		RetrievedAt:   at.Add(time.Hour),
	}}
}

// 与旧 messages 字段一致，刻意不包含 citations。
type stageLegacyMessage struct {
	ID             uint64 `gorm:"primaryKey"`
	ConversationID uint64 `gorm:"not null;index"`
	Role           string `gorm:"size:16;not null"`
	Content        string `gorm:"type:longtext;not null"`
	CreatedAt      time.Time
}

func (stageLegacyMessage) TableName() string { return "messages" }

func TestRAGMessageMySQL(t *testing.T) {
	db := stageDB(t)
	ctx := context.Background()

	stageMust(t, db.AutoMigrate(
		&model.User{},
		&model.Conversation{},
		&stageLegacyMessage{},
	))

	user := model.User{
		Username:     "synthetic-migration-user",
		PasswordHash: "test-only-unused-hash",
	}
	stageMust(t, db.Create(&user).Error)

	conversation := model.Conversation{
		UserID: user.ID,
		Title:  "【虚构测试】迁移与事务",
	}
	stageMust(t, db.Omit("User").Create(&conversation).Error)

	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	old := []stageLegacyMessage{
		{
			ConversationID: conversation.ID,
			Role:           "user",
			Content:        "旧问题🙂",
			CreatedAt:      at,
		},
		{
			ConversationID: conversation.ID,
			Role:           "assistant",
			Content:        "旧回答，不回填引用",
			CreatedAt:      at,
		},
	}
	for i := range old {
		stageMust(t, db.Create(&old[i]).Error)
	}

	messages := repository.NewMessageRepository(db)

	if !t.Run("migration", func(t *testing.T) {
		var beforeColumn int64
		stageMust(t, db.Raw(`
SELECT COUNT(*) FROM information_schema.columns
WHERE table_schema=DATABASE()
  AND table_name='messages' AND column_name='citations'
`).Scan(&beforeColumn).Error)
		stageRequire(t, beforeColumn == 0, "legacy table already has citations")

		stageMust(t, database.Migrate(db))

		var column struct {
			DataType      string
			IsNullable    string
			ColumnDefault sql.NullString
		}
		stageMust(t, db.Raw(`
SELECT DATA_TYPE, IS_NULLABLE, COLUMN_DEFAULT
FROM information_schema.columns
WHERE table_schema=DATABASE()
  AND table_name='messages' AND column_name='citations'
`).Scan(&column).Error)
		stageRequire(t,
			column.DataType == "json" &&
				column.IsNullable == "YES" &&
				!column.ColumnDefault.Valid,
			"citations must be nullable JSON with SQL NULL default",
		)

		var engine string
		stageMust(t, db.Raw(`
SELECT ENGINE FROM information_schema.tables
WHERE table_schema=DATABASE() AND table_name='messages'
`).Scan(&engine).Error)
		stageRequire(t, engine == "InnoDB", "messages must use InnoDB")

		var nullRows int64
		stageMust(t, db.Model(&model.Message{}).
			Where("citations IS NULL").Count(&nullRows).Error)
		stageRequire(t, nullRows == 2, "legacy rows must remain SQL NULL")

		got, err := messages.ListByConversationID(ctx, conversation.ID)
		stageMust(t, err)
		stageRequire(t, len(got) == 2, "legacy row count changed")
		for i := range got {
			stageRequire(t,
				got[i].ID == old[i].ID &&
					got[i].ConversationID == old[i].ConversationID &&
					got[i].Role == old[i].Role &&
					got[i].Content == old[i].Content &&
					got[i].CreatedAt.Equal(old[i].CreatedAt) &&
					len(got[i].Citations) == 0,
				"legacy message changed during migration",
			)
			raw, err := json.Marshal(got[i])
			stageMust(t, err)
			stageRequire(t, bytes.Contains(raw, []byte(`"citations":[]`)),
				"legacy API JSON must expose an empty array")
		}

		stageMust(t, database.Migrate(db))
		again, err := messages.ListByConversationID(ctx, conversation.ID)
		stageMust(t, err)
		stageRequire(t, reflect.DeepEqual(got, again),
			"second migration changed legacy data")

		stageSave(t, "mysql-migration.json", map[string]any{
			"column": column, "engine": engine,
			"legacy_null_rows": nullRows, "legacy_messages": again,
		})
	}) {
		return
	}

	t.Run("round_trip", func(t *testing.T) {
		input := stageCitations()
		want := input.Clone()
		userMessage := model.Message{
			ConversationID: conversation.ID,
			Role:           "user",
			Content:        "【虚构测试】新问题",
		}
		assistant := model.Message{
			ConversationID: conversation.ID,
			Role:           "assistant",
			Content:        "【虚构测试】正文与引用一起保存 [R1]",
			Citations:      input,
		}

		stageMust(t, messages.CreatePair(ctx, &userMessage, &assistant))
		stageRequire(t, userMessage.ID > 0 && assistant.ID > userMessage.ID,
			"pair IDs were not assigned")

		input[0].ChunkIDs[0] = "999"
		*input[0].ExpiresAt = input[0].ExpiresAt.Add(time.Hour)
		stageRequire(t, reflect.DeepEqual(assistant.Citations, want),
			"repository did not freeze the input citation snapshot")

		var stored model.Message
		stageMust(t, db.First(&stored, assistant.ID).Error)
		stageRequire(t,
			stored.Content == assistant.Content &&
				reflect.DeepEqual(stored.Citations, want),
			"content or citations changed in SQL round trip",
		)

		var jsonType, documentID string
		stageMust(t, db.Raw(`
SELECT JSON_TYPE(JSON_EXTRACT(citations,'$[0].document_id'))
FROM messages WHERE id=?
`, assistant.ID).Scan(&jsonType).Error)
		stageMust(t, db.Raw(`
SELECT JSON_UNQUOTE(JSON_EXTRACT(citations,'$[0].document_id'))
FROM messages WHERE id=?
`, assistant.ID).Scan(&documentID).Error)
		stageRequire(t,
			jsonType == "STRING" && documentID == want[0].DocumentID,
			"large citation ID was not preserved as a SQL JSON string",
		)

		var userNull int64
		stageMust(t, db.Model(&model.Message{}).
			Where("id=? AND citations IS NULL", userMessage.ID).
			Count(&userNull).Error)
		stageRequire(t, userNull == 1, "user citations must be SQL NULL")

		stageSave(t, "mysql-round-trip.json", stored)
	})

	t.Run("second_insert_rollback", func(t *testing.T) {
		before := stageCount(t, db)
		first := model.Message{
			ConversationID: conversation.ID,
			Role:           "user",
			Content:        "【虚构测试】必须回滚",
		}
		second := model.Message{
			ID:             old[0].ID,
			ConversationID: conversation.ID,
			Role:           "assistant",
			Content:        "【虚构测试】重复主键",
			Citations:      stageCitations(),
		}

		err := messages.CreatePair(ctx, &first, &second)
		var mysqlErr *driver.MySQLError
		stageRequire(t,
			errors.As(err, &mysqlErr) && mysqlErr.Number == 1062,
			"expected the second INSERT to fail with duplicate primary key",
		)
		stageRequire(t, first.ID != 0,
			"first INSERT must have occurred before the duplicate-key failure")

		var remaining int64
		stageMust(t, db.Model(&model.Message{}).
			Where("id=?", first.ID).Count(&remaining).Error)
		stageRequire(t,
			remaining == 0 && stageCount(t, db) == before,
			"first INSERT survived the failed transaction",
		)

		stageSave(t, "mysql-second-insert-rollback.json", map[string]any{
			"mysql_error":          mysqlErr.Number,
			"first_memory_id":      first.ID,
			"first_committed_rows": remaining,
			"before_count":         before, "after_count": stageCount(t, db),
		})
	})

	t.Run("canceled_before", func(t *testing.T) {
		before := stageCount(t, db)
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		first := model.Message{
			ConversationID: conversation.ID, Role: "user", Content: "取消前",
		}
		second := model.Message{
			ConversationID: conversation.ID, Role: "assistant",
			Content: "不应保存", Citations: stageCitations(),
		}
		err := messages.CreatePair(canceled, &first, &second)
		stageRequire(t, errors.Is(err, context.Canceled),
			"already canceled transaction must fail with cancellation")
		stageRequire(t, stageCount(t, db) == before,
			"already canceled request wrote messages")
	})

	t.Run("canceled_after_first", func(t *testing.T) {
		before := stageCount(t, db)
		requestCtx, cancel := context.WithCancel(ctx)
		defer cancel()

		first := model.Message{
			ConversationID: conversation.ID, Role: "user", Content: "第一条后取消",
		}
		second := model.Message{
			ConversationID: conversation.ID, Role: "assistant",
			Content: "不应提交", Citations: stageCitations(),
		}

		var seenInside int64
		var observationErr error
		const hook = "rag_stage_cancel_after_first"
		stageMust(t, db.Callback().Create().After("gorm:create").
			Register(hook, func(tx *gorm.DB) {
				if tx.Statement.Table != "messages" ||
					tx.Statement.Dest != &first || tx.Error != nil {
					return
				}
				observationErr = tx.Session(&gorm.Session{NewDB: true}).
					Raw("SELECT COUNT(*) FROM messages WHERE id=?", first.ID).
					Scan(&seenInside).Error
				cancel()
			}))
		t.Cleanup(func() {
			stageMust(t, db.Callback().Create().Remove(hook))
		})

		err := messages.CreatePair(requestCtx, &first, &second)
		stageMust(t, observationErr)
		stageRequire(t,
			seenInside == 1 && first.ID > 0 &&
				requestCtx.Err() == context.Canceled && err != nil,
			"cancellation was not established after a real first INSERT",
		)
		stageRequire(t, stageCount(t, db) == before,
			"canceled transaction left committed messages")

		stageSave(t, "mysql-cancel-after-first.json", map[string]any{
			"visible_inside_transaction": seenInside,
			"before_count":               before, "after_count": stageCount(t, db),
		})
	})

	t.Run("invalid_user_citation", func(t *testing.T) {
		before := stageCount(t, db)
		first := model.Message{
			ConversationID: conversation.ID, Role: "user",
			Content: "非法用户引用", Citations: stageCitations(),
		}
		second := model.Message{
			ConversationID: conversation.ID, Role: "assistant", Content: "不应保存",
		}
		err := messages.CreatePair(ctx, &first, &second)
		stageRequire(t, errors.Is(err, model.ErrInvalidMessageCitations),
			"user message citations must be rejected")
		stageRequire(t, stageCount(t, db) == before,
			"invalid pair changed committed data")
	})
}
