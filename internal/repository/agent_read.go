package repository

import (
	"context"

	"video-support-agent/internal/model"
	"video-support-agent/internal/service"

	"gorm.io/gorm"
)

var _ service.AgentMessageStateStore = (*AgentAuditRepository)(nil)
var _ service.AgentReadStore = (*AgentAuditRepository)(nil)

func (r *AgentAuditRepository) ListRunsOwned(ctx context.Context, owner service.AgentAuditOwner, ids []string) ([]model.AgentRun, error) {
	if err := owner.Validate(); err != nil {
		return nil, err
	}
	if len(ids) == 0 || len(ids) > service.AgentReadBatchSize {
		return nil, service.ErrInvalidArgument
	}

	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if err := service.ValidateAppealOperationKey(id); err != nil {
			return nil, err
		}
		if seen[id] {
			return nil, service.ErrInvalidArgument
		}
		seen[id] = true
	}

	runs := make([]model.AgentRun, 0, len(ids))
	err := r.transaction(ctx, true, func(tx *gorm.DB) error {
		if err := appealConversation(tx, owner.UserID, owner.ConversationID, false); err != nil {
			return err
		}

		return tx.
			Where("user_id = ? AND conversation_id = ? AND id IN ?", owner.UserID, owner.ConversationID, ids).
			Order("id").
			Find(&runs).Error
	})
	if err != nil {
		return nil, err
	}
	return runs, nil
}

func (r *AgentAuditRepository) ReadMessageReceipt(ctx context.Context, owner service.AgentAuditOwner, runID string) (*service.AgentMessageReceipt, error) {
	var receipt *service.AgentMessageReceipt

	err := r.transaction(ctx, true, func(tx *gorm.DB) error {
		run, err := auditRun(tx, owner, runID, false)
		if err != nil {
			return err
		}

		var messages []model.Message
		if err := tx.
			Select("id", "conversation_id", "role", "agent_run_id").
			Where("agent_run_id = ?", runID).
			Order("id").
			Limit(3).
			Find(&messages).Error; err != nil {
			return err
		}

		if len(messages) == 0 {
			if run.MessageSaveStatus == "saved" {
				return service.ErrAgentAuditUnavailable
			}
			return nil
		}

		if len(messages) != 2 || run.MessageSaveStatus != "saved" {
			return service.ErrAgentAuditUnavailable
		}

		value := &service.AgentMessageReceipt{}
		for _, message := range messages {
			if message.ID == 0 ||
				message.ConversationID != owner.ConversationID ||
				message.AgentRunID == nil ||
				*message.AgentRunID != runID {
				return service.ErrAgentAuditUnavailable
			}

			switch message.Role {
			case "user":
				if value.UserMessageID != 0 {
					return service.ErrAgentAuditUnavailable
				}
				value.UserMessageID = message.ID

			case "assistant":
				if value.AssistantMessageID != 0 {
					return service.ErrAgentAuditUnavailable
				}
				value.AssistantMessageID = message.ID

			default:
				return service.ErrAgentAuditUnavailable
			}
		}

		if value.UserMessageID == 0 || value.AssistantMessageID == 0 {
			return service.ErrAgentAuditUnavailable
		}

		receipt = value
		return nil
	})
	if err != nil {
		return nil, err
	}
	return receipt, nil
}

func (r *AgentAuditRepository) MarkMessageSaveUnknown(ctx context.Context, owner service.AgentAuditOwner, runID string) error {
	return r.transaction(ctx, false, func(tx *gorm.DB) error {
		run, err := auditRun(tx, owner, runID, true)
		if err != nil {
			return err
		}

		switch run.MessageSaveStatus {
		case "saved":
			return service.ErrAgentAuditConflict

		case "unknown":
			return nil

		case "not_attempted", "failed":
			return auditUpdated(
				tx.Model(&model.AgentRun{}).
					Where("id = ? AND message_save_status = ?", runID, run.MessageSaveStatus).
					Update("message_save_status", "unknown"),
			)

		default:
			return service.ErrAgentAuditConflict
		}
	})
}
