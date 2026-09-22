package repository

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

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
}
