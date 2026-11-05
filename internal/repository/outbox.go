package repository

import (
	"context"
	"errors"
	"time"

	"video-support-agent/internal/knowledgeevent"
	"video-support-agent/internal/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrOutboxLeaseLost = errors.New("outboxrelay lease lost")
	ErrOutboxArgument  = errors.New("invalid outboxrelay argument")
)

const OutboxLeaseDuration = 90 * time.Second

const (
	OutboxDeliveryUnknown  = "delivery_unknown"
	OutboxNotSubmitted     = "not_submitted"
	OutboxInvalidRecord    = "invalid_record"
	OutboxPayloadMismatch  = "payload_mismatch"
	OutboxAttemptsOverflow = "attempts_exhausted"
)

type OutboxRepository struct {
	db *gorm.DB
}

func NewOutboxRepository(db *gorm.DB) (*OutboxRepository, error) {
	if db == nil {
		return nil, ErrOutboxArgument
	}
	return &OutboxRepository{db: db}, nil
}

func (r *OutboxRepository) ClaimOne(ctx context.Context) (*model.EventOutbox, error) {
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	var claimed *model.EventOutbox
	err := r.db.WithContext(bounded).Transaction(func(tx *gorm.DB) error {
		var row model.EventOutbox
		err := tx.Clauses(clause.Locking{
			Strength: "UPDATE",
			Options:  "SKIP LOCKED",
		}).
			Where("(status = ? AND next_attempt_at <= UTC_TIMESTAMP(3)) OR (status = ? AND lease_until <= UTC_TIMESTAMP(3))", model.OutboxPending, model.OutboxSending).
			Order("id ASC").Take(&row).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return err
		}

		if row.Attempts == ^uint64(0) {
			return tx.Model(&model.EventOutbox{}).
				Where("id = ?", row.ID).
				Updates(map[string]any{
					"status":          model.OutboxBlocked,
					"safe_error_code": OutboxAttemptsOverflow,
					"owner_token":     nil,
					"lease_until":     nil,
					"updated_at":      gorm.Expr("UTC_TIMESTAMP(3)"),
				}).Error
		}

		token, err := knowledgeevent.NewID()
		if err != nil {
			return err
		}

		result := tx.Model(&model.EventOutbox{}).
			Where("id = ?", row.ID).
			Updates(map[string]any{
				"status":      model.OutboxSending,
				"attempts":    row.Attempts + 1,
				"owner_token": token,
				"lease_until": gorm.Expr(
					"TIMESTAMPADD(SECOND, ?, UTC_TIMESTAMP(3))",
					int64(OutboxLeaseDuration/time.Second),
				),
				"updated_at": gorm.Expr("UTC_TIMESTAMP(3)"),
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrOutboxLeaseLost
		}

		if err := tx.Where("id = ?", row.ID).First(&row).Error; err != nil {
			return err
		}

		claimed = &row

		return nil
	})
	if err != nil {
		return nil, err
	}

	return claimed, nil
}

func (r *OutboxRepository) MarkSent(ctx context.Context, messageID, owner string) error {
	return r.finish(ctx, messageID, owner, map[string]any{
		"status":          model.OutboxSent,
		"sent_at":         gorm.Expr("UTC_TIMESTAMP(3)"),
		"safe_error_code": "",
	})
}

func (r *OutboxRepository) MarkRetry(ctx context.Context, messageID, owner, code string, delay time.Duration) error {
	if (code != OutboxDeliveryUnknown && code != OutboxNotSubmitted) || delay < time.Second || delay > 10*time.Minute || delay%time.Millisecond != 0 {
		return ErrOutboxArgument
	}

	return r.finish(ctx, messageID, owner, map[string]any{
		"status":          model.OutboxPending,
		"safe_error_code": code,
		"next_attempt_at": gorm.Expr(
			"TIMESTAMPADD(MICROSECOND, ?, UTC_TIMESTAMP(3))",
			delay.Microseconds(),
		),
	})
}

func (r *OutboxRepository) MarkBlocked(ctx context.Context, messageID, owner, code string) error {
	if code != OutboxInvalidRecord && code != OutboxPayloadMismatch {
		return ErrOutboxArgument
	}

	return r.finish(ctx, messageID, owner, map[string]any{
		"status":          model.OutboxBlocked,
		"safe_error_code": code,
	})
}

func (r *OutboxRepository) finish(ctx context.Context, messageID, owner string, updates map[string]any) error {
	if messageID == "" || owner == "" {
		return ErrOutboxArgument
	}

	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	return r.db.WithContext(bounded).Transaction(func(tx *gorm.DB) error {
		var row model.EventOutbox
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Select("id").
			Where("message_id = ?", messageID).
			Take(&row).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrOutboxLeaseLost
		}
		if err != nil {
			return err
		}

		updates["owner_token"] = nil
		updates["lease_until"] = nil
		updates["updated_at"] = gorm.Expr("UTC_TIMESTAMP(3)")

		result := tx.Model(&model.EventOutbox{}).
			Where("id = ? AND message_id = ? AND status = ? AND owner_token = ? AND lease_until > UTC_TIMESTAMP(3)", row.ID, messageID, model.OutboxSending, owner).
			Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrOutboxLeaseLost
		}

		return nil
	})
}
