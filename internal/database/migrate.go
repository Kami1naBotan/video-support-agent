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
		&model.KnowledgeDocument{},
		&model.RuleCategory{},
		&model.RuleVersion{},
		&model.RuleArticle{},
		&model.KnowledgeChunk{},
		&model.RuleVerification{},
		&model.RuleOperation{},
		&model.EventOutbox{},
		&model.KnowledgeIndexControl{},
		&model.KnowledgeIndexBootstrap{},
		&model.KnowledgeEventInbox{},
		&model.KnowledgeIndexAttempt{},
		&model.KnowledgeIndexTask{},
		&model.KnowledgeIndexGeneration{},
		&model.KnowledgeDocumentIndex{},
		&model.KnowledgeBuildRecovery{},
		&model.KnowledgeBuildEvent{},
		&model.KnowledgeRetryReceipt{},
		&model.KnowledgePoison{},
		&model.AppealTicket{},
		&model.TicketRecord{},
		&model.AgentRun{},
		&model.Message{},
		&model.ToolCall{},
	)
	if err != nil {
		return fmt.Errorf("migrate database: %w", err)
	}

	return initializeKnowledgeIndex(db)
}
