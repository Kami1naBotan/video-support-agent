package repository

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"
	"video-support-agent/internal/service"

	"video-support-agent/internal/config"
	"video-support-agent/internal/database"
	"video-support-agent/internal/model"
)

func TestMessageRepositoryMySQL(t *testing.T) {
	if os.Getenv("RUN_MYSQL_TESTS") != "1" {
		t.Skip("set RUN_MYSQL_TESTS=1 to run MySQL integration tests")
	}

	cfg := config.Load()
	if cfg.Database.Name != "bili_support_test" {
		t.Fatal("integration tests require DB_NAME=bili_support_test")
	}

	db, err := database.OpenMySQL(cfg.Database)
	if err != nil {
		t.Fatalf("connect database: %v", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("get connection pool: %v", err)
	}
	t.Cleanup(func() {
		_ = sqlDB.Close()
	})

	if err := database.Migrate(db); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}

	tx := db.Begin()
	if tx.Error != nil {
		t.Fatalf("begin transaction: %v", tx.Error)
	}
	t.Cleanup(func() {
		if err := tx.Rollback().Error; err != nil {
			t.Errorf("rollback test transaction: %v", err)
		}
	})

	ctx := context.Background()
	user := &model.User{
		Username: fmt.Sprintf("test-%d", time.Now().UnixNano()),
	}
	if err := tx.WithContext(ctx).Create(user).Error; err != nil {
		t.Fatalf("create test user: %v", err)
	}

	conversations := NewConversationRepository(tx)
	messages := NewMessageRepository(tx)
	conversation := &model.Conversation{
		UserID: user.ID,
		Title:  "数据库集成测试",
	}
	if err := conversations.Create(ctx, conversation); err != nil {
		t.Fatalf("create conversation: %v", err)
	}

	initial, err := messages.ListByConversationID(ctx, conversation.ID)
	if err != nil {
		t.Fatalf("list initial messages: %v", err)
	}
	if len(initial) != 0 {
		t.Fatalf("expected no initial messages, got %d", len(initial))
	}

	t.Run("latest ID of empty conversation", func(t *testing.T) {
		latestID, err := messages.LatestIDByConversationID(ctx, conversation.ID)
		if err != nil {
			t.Fatalf("query latest ID of empty conversation: %v", err)
		}
		if latestID != 0 {
			t.Fatalf("expected latest ID 0, got %d", latestID)
		}
	})

	want := &model.Message{
		ConversationID: conversation.ID,
		Role:           "user",
		Content:        "账号申诉咨询🙂\n请保留完整正文。",
	}
	if err := messages.Create(ctx, want); err != nil {
		t.Fatalf("create message: %v", err)
	}
	if want.ID == 0 {
		t.Fatal("expected database-generated message id")
	}

	t.Run("latest ID after insert", func(t *testing.T) {
		latestID, err := messages.LatestIDByConversationID(ctx, conversation.ID)
		if err != nil {
			t.Fatalf("query latest ID after insert: %v", err)
		}
		if latestID != want.ID {
			t.Fatalf("expected latest ID %d, got %d", want.ID, latestID)
		}
	})

	got, err := messages.ListByConversationID(ctx, conversation.ID)
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 message, got %d", len(got))
	}

	if got[0].ID != want.ID {
		t.Fatalf("expected id %d, got %d", want.ID, got[0].ID)
	}
	if got[0].ConversationID != want.ConversationID {
		t.Fatalf("expected conversation id %d, got %d", want.ConversationID, got[0].ConversationID)
	}
	if got[0].Role != want.Role {
		t.Fatalf("expected role %q, got %q", want.Role, got[0].Role)
	}
	if got[0].Content != want.Content {
		t.Fatalf("expected content %q, got %q", want.Content, got[0].Content)
	}

	t.Run("history query boundaries and isolation", func(t *testing.T) {
		// 使用独立会话，避免改变外层测试已有的数据预期。
		target := &model.Conversation{
			UserID: user.ID,
			Title:  "历史查询目标会话",
		}
		other := &model.Conversation{
			UserID: user.ID,
			Title:  "历史查询隔离会话",
		}

		for _, item := range []*model.Conversation{target, other} {
			if err := conversations.Create(ctx, item); err != nil {
				t.Fatalf("create history test conversation: %v", err)
			}
		}

		empty, err := messages.ListRecentByConversationID(ctx, target.ID, 20)
		if err != nil {
			t.Fatalf("query empty history: %v", err)
		}
		if len(empty) != 0 {
			t.Fatalf("expected empty history, got %d messages", len(empty))
		}

		// 超过最大合法 limit=100，才能验证上限确实截断了结果。
		inserted := make([]model.Message, 105)
		for i := range inserted {
			inserted[i] = model.Message{
				ConversationID: target.ID,
				Role:           "user",
				Content:        fmt.Sprintf("历史消息 %d", i),
			}
			if err := messages.Create(ctx, &inserted[i]); err != nil {
				t.Fatalf("create history message %d: %v", i, err)
			}
		}

		// 最后插入其他会话消息，使其 ID 大于目标会话的最新 ID。
		foreign := &model.Message{
			ConversationID: other.ID,
			Role:           "user",
			Content:        "这条消息不能出现在目标会话中",
		}
		if err := messages.Create(ctx, foreign); err != nil {
			t.Fatalf("create other conversation message: %v", err)
		}

		wantLatest := inserted[len(inserted)-1].ID
		if foreign.ID <= wantLatest {
			t.Fatal("test setup requires the other conversation's ID to be larger")
		}

		t.Run("latest ID respects conversation", func(t *testing.T) {
			latestID, err := messages.LatestIDByConversationID(ctx, target.ID)
			if err != nil {
				t.Fatalf("query latest ID: %v", err)
			}
			if latestID != wantLatest {
				t.Fatalf("expected latest ID %d, got %d", wantLatest, latestID)
			}
		})

		for _, limit := range []int{1, 3, 20, 100} {
			t.Run(fmt.Sprintf("recent limit %d", limit), func(t *testing.T) {
				got, err := messages.ListRecentByConversationID(ctx, target.ID, limit)
				if err != nil {
					t.Fatalf("query recent messages: %v", err)
				}
				if len(got) != limit {
					t.Fatalf("expected %d messages, got %d", limit, len(got))
				}

				expected := inserted[len(inserted)-limit:]
				for i, message := range got {
					if message.ConversationID != target.ID {
						t.Fatalf("message %d belongs to another conversation", i)
					}
					if message.ID != expected[i].ID ||
						message.Content != expected[i].Content {
						t.Fatalf(
							"unexpected message at index %d: expected ID %d, got %d",
							i, expected[i].ID, message.ID,
						)
					}
				}
			})
		}

		t.Run("limit larger than available history", func(t *testing.T) {
			got, err := messages.ListRecentByConversationID(ctx, other.ID, 20)
			if err != nil {
				t.Fatalf("query short history: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("expected 1 message, got %d", len(got))
			}
			if got[0].ID != foreign.ID {
				t.Fatalf("expected ID %d, got %d", foreign.ID, got[0].ID)
			}
		})

		for _, limit := range []int{-1, 0, 101} {
			t.Run(fmt.Sprintf("reject limit %d", limit), func(t *testing.T) {
				if _, err := messages.ListRecentByConversationID(
					ctx, target.ID, limit,
				); err == nil {
					t.Fatal("invalid limit must return an error")
				}
			})
		}

		t.Run("reject zero conversation ID", func(t *testing.T) {
			if _, err := messages.ListRecentByConversationID(ctx, 0, 20); err == nil {
				t.Fatal("recent query must reject zero conversation ID")
			}
			if _, err := messages.LatestIDByConversationID(ctx, 0); err == nil {
				t.Fatal("latest ID query must reject zero conversation ID")
			}
		})
	})

	t.Run("pair rollback on second insert failure", func(t *testing.T) {
		before, err := messages.ListByConversationID(ctx, conversation.ID)
		if err != nil {
			t.Fatal(err)
		}

		userMessage := &model.Message{
			ConversationID: conversation.ID,
			Role:           "user",
			Content:        "这条消息应该被回滚",
		}

		assistantMessage := &model.Message{
			ID:             want.ID, // 故意复用已存在主键，让第二次插入失败
			ConversationID: conversation.ID,
			Role:           "assistant",
			Content:        "触发重复主键",
		}

		if err := messages.CreatePair(ctx, userMessage, assistantMessage); err == nil {
			t.Fatal("expected duplicate primary key error")
		}

		after, err := messages.ListByConversationID(ctx, conversation.ID)
		if err != nil {
			t.Fatal(err)
		}

		if len(after) != len(before) {
			t.Fatalf("rollback failed: before=%d after=%d", len(before), len(after))
		}

		for _, message := range after {
			if message.ID == userMessage.ID {
				t.Fatal("first insert survived the failed transaction")
			}
		}
	})
}

func TestUserRepositoryDuplicateUsernameMySQL(t *testing.T) {
	if os.Getenv("RUN_MYSQL_TESTS") != "1" {
		t.Skip("set RUN_MYSQL_TESTS=1 to run MySQL integration tests")
	}

	cfg := config.Load()
	if cfg.Database.Name != "bili_support_test" {
		t.Fatal("integration tests require DB_NAME=bili_support_test")
	}

	db, err := database.OpenMySQL(cfg.Database)
	if err != nil {
		t.Fatalf("connect database: %v", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("get connection pool: %v", err)
	}
	t.Cleanup(func() {
		_ = sqlDB.Close()
	})

	if err := database.Migrate(db); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}

	tx := db.Begin()
	if tx.Error != nil {
		t.Fatalf("begin transaction: %v", tx.Error)
	}
	t.Cleanup(func() {
		if err := tx.Rollback().Error; err != nil {
			t.Errorf("rollback test transaction: %v", err)
		}
	})

	ctx := context.Background()

	users := NewUserRepository(tx)

	username := fmt.Sprintf("test-%d", time.Now().UnixNano())
	first := &model.User{
		Username: username,
	}
	if err := users.Create(ctx, first); err != nil {
		t.Fatalf("create first user: %v", err)
	}
	if first.ID == 0 {
		t.Fatal("expected database-generated user id")
	}

	duplicate := &model.User{
		Username: username,
	}
	err = users.Create(ctx, duplicate)

	if !errors.Is(err, service.ErrUsernameExists) {
		t.Fatalf("expected errors.Is(err, service.ErrUsernameExists), got %v", err)
	}
}
