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

func TestConversationRepositoryListMySQL(t *testing.T) {
	if os.Getenv("RUN_MYSQL_TESTS") != "1" {
		t.Skip("set RUN_MYSQL_TESTS=1 to run MySQL integration tests")
	}

	cfg := config.Load()
	if cfg.Database.Name != "bili_support_test" {
		t.Fatal("integration tests require DB_NAME=bili_support_test")
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

	if err := database.Migrate(db); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	tx := db.WithContext(ctx).Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	defer func() {
		_ = tx.Rollback().Error
	}()

	suffix := time.Now().UnixNano()

	userA := &model.User{
		Username: fmt.Sprintf("list-a-%d", suffix),
	}
	userB := &model.User{
		Username: fmt.Sprintf("list-b-%d", suffix),
	}
	userEmpty := &model.User{
		Username: fmt.Sprintf("list-empty-%d", suffix),
	}

	for _, user := range []*model.User{userA, userB, userEmpty} {
		if err := tx.Create(user).Error; err != nil {
			t.Fatal(err)
		}
	}

	repository := NewConversationRepository(tx)

	firstA := &model.Conversation{UserID: userA.ID, Title: "A first"}
	onlyB := &model.Conversation{UserID: userB.ID, Title: "B only"}
	secondA := &model.Conversation{UserID: userA.ID, Title: "A second"}

	for _, conversation := range []*model.Conversation{
		firstA,
		onlyB,
		secondA,
	} {
		if err := repository.Create(ctx, conversation); err != nil {
			t.Fatal(err)
		}
	}

	gotA, err := repository.ListByUserID(ctx, userA.ID)
	if err != nil {
		t.Fatal(err)
	}

	if len(gotA) != 2 {
		t.Fatalf("expected 2 conversations for A, got %d", len(gotA))
	}

	if gotA[0].ID != secondA.ID || gotA[1].ID != firstA.ID {
		t.Fatalf("unexpected descending order: %+v", gotA)
	}

	for _, conversation := range gotA {
		if conversation.UserID != userA.ID {
			t.Fatal("user A received another user's conversation")
		}
	}

	gotB, err := repository.ListByUserID(ctx, userB.ID)
	if err != nil {
		t.Fatal(err)
	}

	if len(gotB) != 1 || gotB[0].ID != onlyB.ID {
		t.Fatalf("unexpected conversations for B: %+v", gotB)
	}

	empty, err := repository.ListByUserID(ctx, userEmpty.ID)
	if err != nil {
		t.Fatal(err)
	}

	if empty == nil || len(empty) != 0 {
		t.Fatalf("expected non-nil empty slice, got %+v", empty)
	}
}
