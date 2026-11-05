package repository

import (
	"errors"
	"fmt"
	"time"

	"video-support-agent/internal/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func (r *KnowledgeRepository) advanceKnowledgeEpoch(tx *gorm.DB) error {
	if err := requireEventTransaction(tx); err != nil {
		return err
	}

	var control model.KnowledgeIndexControl
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", model.KnowledgeIndexControlID).First(&control).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return errors.New("knowledge index control is not initialized")
	}
	if err != nil {
		return fmt.Errorf("lock knowledge index control: %w", err)
	}
	if control.DesiredEpoch == 0 || control.ActiveEpoch > control.DesiredEpoch {
		return errors.New("invalid knowledge index epoch state")
	}
	if control.DesiredEpoch == ^uint64(0) {
		return errors.New("knowledge index epoch limit reached")
	}

	result := tx.Model(&model.KnowledgeIndexControl{}).
		Where("id = ? AND desired_epoch = ?", control.ID, control.DesiredEpoch).
		Updates(map[string]any{
			"desired_epoch": control.DesiredEpoch + 1,
			"updated_at":    r.clock().UTC().Truncate(time.Millisecond),
		})
	if result.Error != nil {
		return fmt.Errorf("advance knowledge index epoch: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return errors.New("knowledge index epoch update conflict")
	}

	return nil
}
