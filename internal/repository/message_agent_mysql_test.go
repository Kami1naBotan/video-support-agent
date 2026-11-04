//go:build auditmysql

package repository

import (
	"context"
	"errors"
	"testing"
	"time"

	"video-support-agent/internal/database"
	"video-support-agent/internal/model"
	"video-support-agent/internal/service"

	driver "github.com/go-sql-driver/mysql"
	"gorm.io/gorm/clause"
)

func messageAgentPair(
	owner service.AgentAuditOwner, runID string,
) (*model.Message, *model.Message) {
	return &model.Message{
		ConversationID: owner.ConversationID,
		Role:           "user",
		Content:        "TEST_ONLY_question",
		AgentRunID:     &runID,
	}, &model.Message{
		ConversationID: owner.ConversationID,
		Role:           "assistant",
		Content:        "TEST_ONLY_answer",
		AgentRunID:     &runID,
	}
}

func messageAgentState(
	t *testing.T, f *auditFixture, runID, status string, count int64,
) {
	t.Helper()

	var run model.AgentRun
	auditMust(t, f.db.WithContext(f.ctx).
		Where("id = ?", runID).Take(&run).Error)

	var actual int64
	auditMust(t, f.db.WithContext(f.ctx).
		Model(&model.Message{}).
		Where("agent_run_id = ?", runID).Count(&actual).Error)

	if run.MessageSaveStatus != status || actual != count {
		t.Fatalf("state=%s messages=%d; want %s/%d",
			run.MessageSaveStatus, actual, status, count)
	}
}

func messageAgentSQLError(t *testing.T, err error, number uint16) {
	t.Helper()
	var mysqlErr *driver.MySQLError
	if !errors.As(err, &mysqlErr) || mysqlErr.Number != number {
		t.Fatalf("error=%v; want MySQL error %d", err, number)
	}
}

func TestAgentMessageMySQL(t *testing.T) {
	db := auditDatabase(t)

	if !t.Run("fresh_migration", func(t *testing.T) {
		auditMust(t, database.Migrate(db))
		auditMust(t, database.Migrate(db))

		for _, table := range []string{"messages", "agent_runs"} {
			var engine string
			auditMust(t, db.Raw(
				"SELECT ENGINE FROM information_schema.TABLES "+
					"WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?",
				table,
			).Scan(&engine).Error)
			if engine != "InnoDB" {
				t.Fatalf("%s uses %s, not InnoDB", table, engine)
			}
		}
	}) {
		return
	}

	t.Run("upgrade_preserves_legacy_messages", func(t *testing.T) {
		old := auditDatabase(t)

		auditMust(t, old.AutoMigrate(
			&model.User{}, &model.Conversation{},
		))
		// 使用旧结构，不引用当前 Message 模型，避免关联自动迁移
		// 提前创建新字段，导致“升级测试”实际上没有测试升级。
		auditMust(t, old.Exec(`
			CREATE TABLE messages (
				id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
				conversation_id BIGINT UNSIGNED NOT NULL,
				role VARCHAR(16) NOT NULL,
				content LONGTEXT NOT NULL,
				citations JSON NULL,
				created_at DATETIME(3) NULL,
				INDEX idx_messages_conversation_id (conversation_id)
			) ENGINE=InnoDB
		`).Error)

		f := auditSeed(t, old)
		auditMust(t, old.Exec(
			"INSERT INTO messages "+
				"(conversation_id, role, content) VALUES (?, ?, ?)",
			f.owner.ConversationID, "user", "TEST_ONLY_legacy",
		).Error)

		if old.Migrator().HasColumn("messages", "agent_run_id") {
			t.Fatal("legacy schema already contains agent_run_id")
		}

		auditMust(t, database.Migrate(old))
		auditMust(t, database.Migrate(old))

		var messages []model.Message
		auditMust(t, old.Where(
			"conversation_id = ?", f.owner.ConversationID,
		).Find(&messages).Error)
		if len(messages) != 1 || messages[0].Content != "TEST_ONLY_legacy" ||
			messages[0].AgentRunID != nil {
			t.Fatalf("legacy message changed: %+v", messages)
		}

		run := f.run()
		user, assistant := messageAgentPair(f.owner, run.ID)
		auditMust(t, NewMessageRepository(old).CreatePair(f.ctx, user, assistant))
		messageAgentState(t, f, run.ID, "saved", 2)
	})

	t.Run("atomic_pair_and_receipt", func(t *testing.T) {
		f := auditSeed(t, db)
		run := f.run()

		receipt, err := f.repo.ReadMessageReceipt(f.ctx, f.owner, run.ID)
		auditMust(t, err)
		if receipt != nil {
			t.Fatal("unsaved Run returned a receipt")
		}

		auditMust(t, f.repo.MarkMessageSaveUnknown(f.ctx, f.owner, run.ID))
		auditMust(t, f.repo.MarkMessageSaveUnknown(f.ctx, f.owner, run.ID))
		messageAgentState(t, f, run.ID, "unknown", 0)

		user, assistant := messageAgentPair(f.owner, run.ID)
		auditMust(t, NewMessageRepository(db).CreatePair(f.ctx, user, assistant))
		messageAgentState(t, f, run.ID, "saved", 2)

		// 使用新仓储实例读取，证明回执来自数据库而非内存对象。
		reader := NewMessageRepository(db)
		rows, err := reader.ListByConversationID(f.ctx, f.owner.ConversationID)
		auditMust(t, err)
		if len(rows) != 2 || rows[0].ID != user.ID || rows[1].ID != assistant.ID ||
			rows[0].Role != "user" || rows[1].Role != "assistant" ||
			rows[0].Content != user.Content || rows[1].Content != assistant.Content {
			t.Fatalf("stored pair mismatch: %+v", rows)
		}

		receipt, err = f.repo.ReadMessageReceipt(f.ctx, f.owner, run.ID)
		auditMust(t, err)
		if receipt == nil || receipt.UserMessageID != user.ID ||
			receipt.AssistantMessageID != assistant.ID {
			t.Fatalf("receipt mismatch: %+v", receipt)
		}

		auditWant(t,
			f.repo.MarkMessageSaveUnknown(f.ctx, f.owner, run.ID),
			service.ErrAgentAuditConflict,
		)
		messageAgentState(t, f, run.ID, "saved", 2)
	})

	for _, point := range []string{"second_insert", "saved_update"} {
		t.Run("rollback_"+point, func(t *testing.T) {
			f := auditSeed(t, db)
			run := f.run()
			auditMust(t, f.repo.MarkMessageSaveUnknown(f.ctx, f.owner, run.ID))

			statement := `
				CREATE TRIGGER message_pair_fault
				BEFORE INSERT ON messages FOR EACH ROW
				BEGIN
					IF NEW.role = 'assistant' THEN
						SIGNAL SQLSTATE '45000'
						SET MESSAGE_TEXT = 'TEST_ONLY_second_insert';
					END IF;
				END
			`
			if point == "saved_update" {
				statement = `
					CREATE TRIGGER message_pair_fault
					BEFORE UPDATE ON agent_runs FOR EACH ROW
					BEGIN
						IF NEW.message_save_status = 'saved' THEN
							SIGNAL SQLSTATE '45000'
							SET MESSAGE_TEXT = 'TEST_ONLY_saved_update';
						END IF;
					END
				`
			}
			auditMust(t, db.Exec(statement).Error)
			t.Cleanup(func() {
				auditMust(t, db.Exec("DROP TRIGGER IF EXISTS message_pair_fault").Error)
			})

			user, assistant := messageAgentPair(f.owner, run.ID)
			err := NewMessageRepository(db).CreatePair(f.ctx, user, assistant)
			messageAgentSQLError(t, err, 1644)
			messageAgentState(t, f, run.ID, "unknown", 0)

			receipt, err := f.repo.ReadMessageReceipt(f.ctx, f.owner, run.ID)
			auditMust(t, err)
			if receipt != nil {
				t.Fatal("rolled-back messages produced a receipt")
			}

			auditMust(t, db.Exec("DROP TRIGGER message_pair_fault").Error)

			// 回滚后的对象可能已有分配的自增ID；重试必须使用新对象。
			user, assistant = messageAgentPair(f.owner, run.ID)
			auditMust(t, NewMessageRepository(db).CreatePair(f.ctx, user, assistant))
			messageAgentState(t, f, run.ID, "saved", 2)
		})
	}

	t.Run("concurrent_pair", func(t *testing.T) {
		f := auditSeed(t, db)
		run := f.run()
		auditMust(t, f.repo.MarkMessageSaveUnknown(f.ctx, f.owner, run.ID))

		auditRace(t, func() error {
			user, assistant := messageAgentPair(f.owner, run.ID)
			return NewMessageRepository(db).CreatePair(f.ctx, user, assistant)
		})
		messageAgentState(t, f, run.ID, "saved", 2)

		receipt, err := f.repo.ReadMessageReceipt(f.ctx, f.owner, run.ID)
		auditMust(t, err)
		if receipt == nil || receipt.UserMessageID == 0 ||
			receipt.AssistantMessageID == 0 ||
			receipt.UserMessageID == receipt.AssistantMessageID {
			t.Fatalf("invalid winning receipt: %+v", receipt)
		}
	})

	t.Run("database_constraints_and_nullable_legacy", func(t *testing.T) {
		f := auditSeed(t, db)
		run := f.run()
		user, assistant := messageAgentPair(f.owner, run.ID)
		auditMust(t, NewMessageRepository(db).CreatePair(f.ctx, user, assistant))

		// 绕过仓储保护，直接证明数据库唯一键和外键生效。
		duplicate, _ := messageAgentPair(f.owner, run.ID)
		err := db.Omit(clause.Associations).Create(duplicate).Error
		messageAgentSQLError(t, err, 1062)

		missingID := auditID(t)
		orphan, _ := messageAgentPair(f.owner, missingID)
		err = db.Omit(clause.Associations).Create(orphan).Error
		messageAgentSQLError(t, err, 1452)

		err = db.Where("id = ?", run.ID).Delete(&model.AgentRun{}).Error
		messageAgentSQLError(t, err, 1451)

		// NULL Run 不应被复合唯一键误伤：旧入口同角色可有多条消息。
		for i := 0; i < 2; i++ {
			message := &model.Message{
				ConversationID: f.owner.ConversationID,
				Role:           "user",
				Content:        "TEST_ONLY_legacy",
			}
			auditMust(t, NewMessageRepository(db).Create(f.ctx, message))
		}
		var count int64
		auditMust(t, db.Model(&model.Message{}).
			Where("conversation_id = ? AND agent_run_id IS NULL", f.owner.ConversationID).
			Count(&count).Error)
		if count != 2 {
			t.Fatalf("legacy NULL Run count=%d", count)
		}
		messageAgentState(t, f, run.ID, "saved", 2)
	})

	t.Run("pair_boundaries", func(t *testing.T) {
		f := auditSeed(t, db)
		run := f.run()
		repo := NewMessageRepository(db)

		for _, fault := range []string{
			"one_run", "different_run", "wrong_role", "different_conversation",
			"foreign_conversation", "existing_id", "same_object",
		} {
			user, assistant := messageAgentPair(f.owner, run.ID)
			switch fault {
			case "one_run":
				assistant.AgentRunID = nil
			case "different_run":
				id := auditID(t)
				assistant.AgentRunID = &id
			case "wrong_role":
				assistant.Role = "user"
			case "different_conversation":
				assistant.ConversationID = f.other.ConversationID
			case "foreign_conversation":
				user.ConversationID = f.other.ConversationID
				assistant.ConversationID = f.other.ConversationID
			case "existing_id":
				user.ID = 999
			case "same_object":
				assistant = user
			}
			if err := repo.CreatePair(f.ctx, user, assistant); err == nil {
				t.Fatalf("accepted invalid pair: %s", fault)
			}
			messageAgentState(t, f, run.ID, "not_attempted", 0)
		}

		user, _ := messageAgentPair(f.owner, run.ID)
		if err := repo.Create(f.ctx, user); err == nil {
			t.Fatal("single-message entry accepted Agent Run")
		}
		messageAgentState(t, f, run.ID, "not_attempted", 0)
	})

	t.Run("owner_isolation", func(t *testing.T) {
		f := auditSeed(t, db)
		run := f.run()
		user, assistant := messageAgentPair(f.owner, run.ID)
		auditMust(t, NewMessageRepository(db).CreatePair(f.ctx, user, assistant))

		owners := []service.AgentAuditOwner{
			f.other,
			{UserID: f.other.UserID, ConversationID: f.owner.ConversationID},
		}
		for _, owner := range owners {
			receipt, err := f.repo.ReadMessageReceipt(f.ctx, owner, run.ID)
			if err == nil || receipt != nil {
				t.Fatal("foreign owner received a receipt")
			}
			if err := f.repo.MarkMessageSaveUnknown(f.ctx, owner, run.ID); err == nil {
				t.Fatal("foreign owner changed save state")
			}
			runs, _ := f.repo.ListRunsOwned(f.ctx, owner, []string{run.ID})
			if len(runs) != 0 {
				t.Fatal("foreign owner received Run summary")
			}
		}

		runs, err := f.repo.ListRunsOwned(f.ctx, f.owner, []string{run.ID})
		auditMust(t, err)
		if len(runs) != 1 || runs[0].ID != run.ID {
			t.Fatal("owner cannot read saved Run")
		}
		messageAgentState(t, f, run.ID, "saved", 2)
	})

	t.Run("lock_deadline_rolls_back", func(t *testing.T) {
		f := auditSeed(t, db)
		run := f.run()
		auditMust(t, f.repo.MarkMessageSaveUnknown(f.ctx, f.owner, run.ID))

		lock := db.Begin()
		auditMust(t, lock.Error)
		defer lock.Rollback()

		var held model.AgentRun
		auditMust(t, lock.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ?", run.ID).Take(&held).Error)

		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		user, assistant := messageAgentPair(f.owner, run.ID)
		err := NewMessageRepository(db).CreatePair(ctx, user, assistant)
		if err == nil || !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatalf("locked save did not expire: %v / %v", err, ctx.Err())
		}

		auditMust(t, lock.Rollback().Error)
		messageAgentState(t, f, run.ID, "unknown", 0)

		user, assistant = messageAgentPair(f.owner, run.ID)
		auditMust(t, NewMessageRepository(db).CreatePair(f.ctx, user, assistant))
		messageAgentState(t, f, run.ID, "saved", 2)
	})

	for _, corruption := range []string{
		"saved_without_pair", "single_message", "pair_without_saved",
		"foreign_message", "unexpected_role",
	} {
		t.Run("receipt_rejects_"+corruption, func(t *testing.T) {
			f := auditSeed(t, db)
			run := f.run()
			user, assistant := messageAgentPair(f.owner, run.ID)

			// 仅在新建测试库内制造不一致数据，检验读取端拒绝伪回执。
			switch corruption {
			case "saved_without_pair":
				auditMust(t, db.Model(&model.AgentRun{}).
					Where("id = ?", run.ID).
					Update("message_save_status", "saved").Error)
			case "single_message":
				auditMust(t, db.Omit(clause.Associations).Create(user).Error)
			case "pair_without_saved":
				auditMust(t, db.Omit(clause.Associations).Create(user).Error)
				auditMust(t, db.Omit(clause.Associations).Create(assistant).Error)
			default:
				auditMust(t, NewMessageRepository(db).CreatePair(f.ctx, user, assistant))
				if corruption == "foreign_message" {
					auditMust(t, db.Model(&model.Message{}).
						Where("id = ?", assistant.ID).
						Update("conversation_id", f.other.ConversationID).Error)
				} else {
					auditMust(t, db.Model(&model.Message{}).
						Where("id = ?", assistant.ID).
						Update("role", "system").Error)
				}
			}

			receipt, err := f.repo.ReadMessageReceipt(f.ctx, f.owner, run.ID)
			auditWant(t, err, service.ErrAgentAuditUnavailable)
			if receipt != nil {
				t.Fatal("inconsistent database returned a receipt")
			}
		})
	}
}
