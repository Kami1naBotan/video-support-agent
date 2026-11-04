package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"video-support-agent/internal/model"
	"video-support-agent/internal/service"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

type AgentAuditRepository struct {
	db    *gorm.DB
	clock Clock
}

var _ service.AgentAuditStore = (*AgentAuditRepository)(nil)

func NewAgentAuditRepository(db *gorm.DB) (*AgentAuditRepository, error) {
	return NewAgentAuditRepositoryWithClock(db, time.Now)
}

func NewAgentAuditRepositoryWithClock(db *gorm.DB, clock Clock) (*AgentAuditRepository, error) {
	if db == nil ||
		db.Error != nil ||
		db.Statement == nil ||
		db.Dialector == nil ||
		db.Dialector.Name() != "mysql" ||
		db.DryRun ||
		db.TranslateError ||
		clock == nil {
		return nil, service.ErrAgentAuditConfiguration
	}

	if _, ok := db.Statement.ConnPool.(*sql.DB); !ok {
		return nil, service.ErrAgentAuditConfiguration
	}

	return &AgentAuditRepository{
		db: db.Session(&gorm.Session{
			NewDB:  true,
			Logger: logger.Default.LogMode(logger.Silent),
		}),
		clock: clock,
	}, nil
}

func auditError(err error) error {
	if err == nil {
		return nil
	}

	for _, known := range []error{
		context.Canceled,
		context.DeadlineExceeded,
		service.ErrUnauthorized,
		service.ErrInvalidArgument,
		service.ErrConversationNotFound,
		service.ErrAgentRunNotFound,
		service.ErrAgentAuditConflict,
		service.ErrAgentAuditConfiguration,
	} {
		if errors.Is(err, known) {
			return known
		}
	}
	return service.ErrAgentAuditUnavailable
}

func (r *AgentAuditRepository) now() (time.Time, error) {
	now := r.clock().UTC().Truncate(time.Millisecond)
	if now.IsZero() || now.Year() < 1000 || now.Year() > 9999 {
		return time.Time{}, service.ErrAgentAuditConfiguration
	}
	return now, nil
}

func (r *AgentAuditRepository) transaction(ctx context.Context, readOnly bool, fn func(*gorm.DB) error) error {
	ctx, release := service.WithAppealFinalization(ctx)
	defer release()

	if err := ctx.Err(); err != nil {
		return err
	}

	work, cancel := context.WithCancel(ctx)
	defer cancel()

	isolation := sql.LevelReadCommitted
	if readOnly {
		isolation = sql.LevelRepeatableRead
	}

	tx := r.db.WithContext(work).Begin(&sql.TxOptions{
		Isolation: isolation,
		ReadOnly:  readOnly,
	})
	if tx.Error != nil {
		return auditError(tx.Error)
	}

	raw := tx.Statement.ConnPool.(*sql.Tx)
	closed := false
	defer func() {
		if !closed {
			_ = appealRollback(ctx, cancel, raw)
		}
	}()

	if err := fn(tx); err != nil {
		return auditError(err)
	}
	if err := work.Err(); err != nil {
		return err
	}

	closed = true
	done := make(chan error, 1)
	go func() {
		done <- raw.Commit()
	}()

	known, err := awaitAppealEnd(ctx, done)
	if !known || err != nil {
		return service.ErrAgentAuditUnavailable
	}
	return nil
}

func auditRun(tx *gorm.DB, owner service.AgentAuditOwner, runID string, lock bool) (*model.AgentRun, error) {
	if err := owner.Validate(); err != nil {
		return nil, err
	}
	if err := service.ValidateAppealOperationKey(runID); err != nil {
		return nil, err
	}
	if err := appealConversation(tx, owner.UserID, owner.ConversationID, lock); err != nil {
		return nil, err
	}

	q := tx.Where("id = ? AND user_id = ? AND conversation_id = ?", runID, owner.UserID, owner.ConversationID)
	if lock {
		q = q.Clauses(clause.Locking{Strength: "UPDATE"})
	}

	var run model.AgentRun
	if err := q.Take(&run).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, service.ErrAgentRunNotFound
		}
		return nil, err
	}
	return &run, nil
}

func auditCall(tx *gorm.DB, runID, callID string) (*model.ToolCall, error) {
	if err := service.ValidateAppealOperationKey(callID); err != nil {
		return nil, err
	}

	var call model.ToolCall
	if err := tx.Where("id = ? AND run_id = ?", callID, runID).Take(&call).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, service.ErrAgentAuditConflict
		}
		return nil, err
	}
	return &call, nil
}

func auditUpdated(result *gorm.DB) error {
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return service.ErrAgentAuditConflict
	}
	return nil
}

func (r *AgentAuditRepository) StartRun(ctx context.Context, owner service.AgentAuditOwner) (*model.AgentRun, error) {
	if err := owner.Validate(); err != nil {
		return nil, err
	}

	id, err := service.NewAgentAuditID()
	if err != nil {
		return nil, service.ErrAgentAuditUnavailable
	}
	now, err := r.now()
	if err != nil {
		return nil, err
	}

	run := model.AgentRun{
		ID:                id,
		UserID:            owner.UserID,
		ConversationID:    owner.ConversationID,
		ExecutionStatus:   "running",
		Decision:          "",
		ReasonCode:        "none",
		BusinessEffect:    "none",
		MessageSaveStatus: "not_attempted",
		StartedAt:         now,
	}

	err = r.transaction(ctx, false, func(tx *gorm.DB) error {
		if err := appealConversation(
			tx, owner.UserID, owner.ConversationID, true,
		); err != nil {
			return err
		}
		return tx.Omit(clause.Associations).Create(&run).Error
	})
	if err != nil {
		return nil, err
	}
	return &run, nil
}

func (r *AgentAuditRepository) StartCall(ctx context.Context, owner service.AgentAuditOwner, runID string, command service.AgentCallStart) (*model.ToolCall, error) {
	if err := service.ValidateAgentCallStart(owner, command); err != nil {
		return nil, err
	}

	id, err := service.NewAgentAuditID()
	if err != nil {
		return nil, service.ErrAgentAuditUnavailable
	}

	var binding service.AppealConfirmationBinding
	if command.Binding != nil {
		binding = *command.Binding
	}

	var call model.ToolCall
	err = r.transaction(ctx, false, func(tx *gorm.DB) error {
		run, err := auditRun(tx, owner, runID, true)
		if err != nil {
			return err
		}
		if run.ExecutionStatus != "running" || run.ToolCalls >= 5 {
			return service.ErrAgentAuditConflict
		}

		var previous []model.ToolCall
		if err := tx.Where("run_id = ?", runID).Order("sequence").Find(&previous).Error; err != nil {
			return err
		}
		if len(previous) != int(run.ToolCalls) {
			return service.ErrAgentAuditConflict
		}

		for _, c := range previous {
			if c.Status != "succeeded" ||
				c.ToolName == "create_appeal_ticket" ||
				c.ToolName == "query_ticket_status" ||
				(c.ModelCallID != nil && *c.ModelCallID == command.ModelCallID) {
				return service.ErrAgentAuditConflict
			}
		}

		now, err := r.now()
		if err != nil {
			return err
		}
		if now.Before(run.StartedAt) {
			return service.ErrAgentAuditConfiguration
		}

		call = model.ToolCall{
			ID:             id,
			RunID:          runID,
			Sequence:       run.ToolCalls + 1,
			ToolName:       command.ToolName,
			Status:         "started",
			ErrorCode:      "none",
			BusinessEffect: "none",
			ArgumentBytes:  command.ArgumentBytes,
			Confirmed:      command.Binding != nil,
			StartedAt:      now,
		}

		if command.ModelCallID != "" {
			value := command.ModelCallID
			call.ModelCallID = &value
		}
		if command.Binding != nil {
			call.OperationKey = binding.IdempotencyKey
			call.RequestHash = binding.RequestHash
			call.PolicyDigest = binding.PolicyDigest
		}

		if err := tx.Omit(clause.Associations).
			Create(&call).Error; err != nil {
			return err
		}

		return auditUpdated(tx.Model(&model.AgentRun{}).
			Where("id = ? AND execution_status = ? AND tool_calls = ?", runID, "running", run.ToolCalls).
			Update("tool_calls", call.Sequence))
	})
	if err != nil {
		return nil, err
	}
	return &call, nil
}

func (r *AgentAuditRepository) AuthorizeCreate(ctx context.Context, owner service.AgentAuditOwner, runID, callID string, binding service.AppealConfirmationBinding) error {
	if err := service.ValidateAppealConfirmationBinding(binding); err != nil {
		return err
	}
	if binding.UserID != owner.UserID || binding.ConversationID != owner.ConversationID {
		return service.ErrInvalidArgument
	}

	return r.transaction(ctx, false, func(tx *gorm.DB) error {
		run, err := auditRun(tx, owner, runID, true)
		if err != nil {
			return err
		}
		call, err := auditCall(tx, runID, callID)
		if err != nil {
			return err
		}

		if run.ExecutionStatus != "running" ||
			call.Status != "started" ||
			call.Sequence != run.ToolCalls ||
			call.ToolName != "create_appeal_ticket" ||
			!call.Confirmed ||
			call.WriteAuthorized ||
			call.OperationKey != binding.IdempotencyKey ||
			call.RequestHash != binding.RequestHash ||
			call.PolicyDigest != binding.PolicyDigest {
			return service.ErrAgentAuditConflict
		}

		if err := auditUpdated(tx.Model(&model.ToolCall{}).
			Where("id = ? AND status = ? AND write_authorized = ?", callID, "started", false).
			Updates(map[string]any{
				"write_authorized": true,
				"business_effect":  "unknown",
			})); err != nil {
			return err
		}

		return auditUpdated(tx.Model(&model.AgentRun{}).
			Where("id = ?", runID).
			Update("business_effect", "unknown"))
	})
}

func (r *AgentAuditRepository) FinishCall(ctx context.Context, owner service.AgentAuditOwner, runID, callID string, finish service.AgentCallFinish) error {
	if err := service.ValidateAgentCallFinish(finish); err != nil {
		return err
	}

	return r.transaction(ctx, false, func(tx *gorm.DB) error {
		run, err := auditRun(tx, owner, runID, true)
		if err != nil {
			return err
		}
		call, err := auditCall(tx, runID, callID)
		if err != nil {
			return err
		}

		if run.ExecutionStatus != "running" || call.Status != "started" {
			return service.ErrAgentAuditConflict
		}
		if finish.BusinessEffect != "none" && !call.WriteAuthorized {
			return service.ErrAgentAuditConflict
		}
		if call.ToolName == "create_appeal_ticket" && finish.Status == "succeeded" && finish.BusinessEffect != "committed" {
			return service.ErrAgentAuditConflict
		}
		if finish.ErrorCode == "confirmation_required" && !call.Confirmed && finish.Status != "rejected" {
			return service.ErrAgentAuditConflict
		}

		var ticketID *uint64
		var ticketAt *time.Time
		var ticketStatus model.AppealTicketStatus
		replayed := false

		if finish.Ticket != nil {
			ticket, err := appealFindOperation(tx, owner.UserID, call.OperationKey)
			if err != nil {
				return err
			}

			saved, err := service.AppealSavedCreationResult(
				ticket,
				service.AppealCreateCommand{
					UserID:         owner.UserID,
					ConversationID: owner.ConversationID,
					IdempotencyKey: call.OperationKey,
					RequestHash:    call.RequestHash,
				},
				finish.Ticket.Replayed,
			)
			if err != nil {
				return err
			}

			if saved.TicketID != finish.Ticket.TicketID || saved.InitialStatus != finish.Ticket.InitialStatus || !saved.CreatedAt.Equal(finish.Ticket.CreatedAt) {
				return service.ErrAgentAuditConflict
			}

			value := saved.TicketID
			ticketID, replayed = &value, saved.Replayed

			at := saved.CreatedAt
			ticketAt, ticketStatus = &at, saved.InitialStatus
		}

		if call.WriteAuthorized && finish.BusinessEffect == "none" {
			_, err := appealFindOperation(tx, owner.UserID, call.OperationKey)
			if !errors.Is(err, service.ErrAppealTicketNotFound) {
				if err != nil {
					return err
				}
				return service.ErrAgentAuditConflict
			}
		}

		now, err := r.now()
		if err != nil {
			return err
		}
		if now.Before(call.StartedAt) {
			return service.ErrAgentAuditConfiguration
		}

		if err := auditUpdated(tx.Model(&model.ToolCall{}).
			Where("id = ? AND status = ?", callID, "started").
			Updates(map[string]any{
				"status":            finish.Status,
				"error_code":        finish.ErrorCode,
				"business_effect":   finish.BusinessEffect,
				"ticket_id":         ticketID,
				"ticket_status":     ticketStatus,
				"ticket_created_at": ticketAt,
				"replayed":          replayed,
				"finished_at":       now,
				"duration_ms":       now.Sub(call.StartedAt).Milliseconds(),
			})); err != nil {
			return err
		}

		if call.WriteAuthorized && run.BusinessEffect != finish.BusinessEffect {
			return auditUpdated(tx.Model(&model.AgentRun{}).Where("id = ?", runID).Update("business_effect", finish.BusinessEffect))
		}
		return nil
	})
}

func (r *AgentAuditRepository) FinishRun(ctx context.Context, owner service.AgentAuditOwner, runID string, finish service.AgentRunFinish) error {
	if err := service.ValidateAgentRunFinish(finish); err != nil {
		return err
	}

	return r.transaction(ctx, false, func(tx *gorm.DB) error {
		run, err := auditRun(tx, owner, runID, true)
		if err != nil {
			return err
		}
		if run.ExecutionStatus != "running" {
			return service.ErrAgentAuditConflict
		}

		var calls []model.ToolCall
		if err := tx.Where("run_id = ?", runID).Find(&calls).Error; err != nil {
			return err
		}

		effect := run.BusinessEffect
		pending := false

		for _, call := range calls {
			if call.Status == "started" {
				pending = true
			}
			if call.BusinessEffect == "committed" {
				effect = "committed"
			}
			if call.WriteAuthorized && call.Status == "started" && effect != "committed" {
				effect = "unknown"
			}
		}

		if finish.Status == "completed" && pending {
			return service.ErrAgentAuditConflict
		}
		if finish.Decision == "ticket_created" && effect != "committed" {
			return service.ErrAgentAuditConflict
		}

		now, err := r.now()
		if err != nil {
			return err
		}
		if now.Before(run.StartedAt) {
			return service.ErrAgentAuditConfiguration
		}

		return auditUpdated(tx.Model(&model.AgentRun{}).
			Where("id = ? AND execution_status = ?", runID, "running").
			Updates(map[string]any{
				"execution_status": finish.Status,
				"decision":         finish.Decision,
				"reason_code":      finish.ReasonCode,
				"business_effect":  effect,
				"model_calls":      finish.ModelCalls,
				"finished_at":      now,
			}))
	})
}

func (r *AgentAuditRepository) ReadOwned(ctx context.Context, owner service.AgentAuditOwner, runID string) (*service.AgentAuditSnapshot, error) {
	var snapshot service.AgentAuditSnapshot

	err := r.transaction(ctx, true, func(tx *gorm.DB) error {
		run, err := auditRun(tx, owner, runID, false)
		if err != nil {
			return err
		}

		snapshot.Run = *run
		snapshot.Calls = make([]model.ToolCall, 0)

		return tx.Where("run_id = ?", runID).
			Order("sequence").
			Find(&snapshot.Calls).Error
	})
	if err != nil {
		return nil, err
	}
	return &snapshot, nil
}
