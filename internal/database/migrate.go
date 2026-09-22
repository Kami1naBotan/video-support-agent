package database

import (
	"fmt"
	"video-support-agent/internal/model"

	"gorm.io/gorm"
)

func Migrate(db *gorm.DB) error {
	err := db.AutoMigrate(
		&model.User{},
		&model.Conversation{},
		&model.Message{},
		&model.KnowledgeDocument{},
		&model.KnowledgeChunk{},
	)
	if err != nil {
		return fmt.Errorf("migrate database: %w", err)
	}

	return nil
}
