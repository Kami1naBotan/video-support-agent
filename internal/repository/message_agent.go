package repository

import (
	"fmt"

	"video-support-agent/internal/model"
	"video-support-agent/internal/service"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func messagePairRunID(userMessage *model.Message, assistantMessage *model.Message) (string, error) {
	if userMessage == nil || assistantMessage == nil {
		return "", fmt.Errorf("message pair is required")
	}

	if userMessage.AgentRunID == nil && assistantMessage.AgentRunID == nil {
		return "", nil
	}

	if userMessage.AgentRunID == nil || assistantMessage.AgentRunID == nil {
		return "", fmt.Errorf("both messages must reference the same agent run")
	}

	runID := *userMessage.AgentRunID
	if runID != *assistantMessage.AgentRunID || !validMessageRunID(runID) {
		return "", fmt.Errorf("invalid message agent run")
	}

	if userMessage.ID != 0 || assistantMessage.ID != 0 {
		return "", fmt.Errorf("agent message pair must contain new messages")
	}

	if userMessage.Role != "user" || assistantMessage.Role != "assistant" {
		return "", fmt.Errorf("invalid agent message roles")
	}

	if userMessage.ConversationID == 0 || userMessage.ConversationID != assistantMessage.ConversationID {
		return "", fmt.Errorf("agent messages must belong to the same conversation")
	}

	return runID, nil
}

func validMessageRunID(value string) bool {
	if len(value) != 36 {
		return false
	}

	for i := 0; i < len(value); i++ {
		switch i {
		case 8, 13, 18, 23:
			if value[i] != '-' {
				return false
			}
		default:
			c := value[i]
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
				return false
			}
		}
	}

	return value[14] == '4' && (value[19] == '8' || value[19] == '9' || value[19] == 'a' || value[19] == 'b')
}

func lockMessageAgentRun(tx *gorm.DB, runID string, conversationID uint64) error {
	var run model.AgentRun

	err := tx.
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ? AND conversation_id = ?", runID, conversationID).
		Take(&run).Error
	if err != nil {
		return fmt.Errorf("lock message agent run: %w", err)
	}

	var conversation model.Conversation
	if err := tx.
		Where("id = ? AND user_id = ?", conversationID, run.UserID).
		Take(&conversation).Error; err != nil {
		return fmt.Errorf("verify message agent owner: %w", err)
	}

	switch run.MessageSaveStatus {
	case "not_attempted", "failed", "unknown":
		return nil
	default:
		return service.ErrAgentAuditConflict
	}
}

func markMessageAgentRunSaved(tx *gorm.DB, runID string, conversationID uint64) error {
	result := tx.
		Model(&model.AgentRun{}).
		Where(
			"id = ? AND conversation_id = ? AND message_save_status IN ?",
			runID,
			conversationID,
			[]string{"not_attempted", "failed", "unknown"},
		).
		Update("message_save_status", "saved")

	if result.Error != nil {
		return fmt.Errorf("mark agent messages saved: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return service.ErrAgentAuditConflict
	}

	return nil
}
