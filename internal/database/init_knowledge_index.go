package database

import (
	"fmt"
	"time"

	"video-support-agent/internal/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func initializeKnowledgeIndex(db *gorm.DB) error {
	err := db.Transaction(func(tx *gorm.DB) error {
		now := time.Now().UTC().Truncate(time.Millisecond)
		control := model.KnowledgeIndexControl{
			ID:           model.KnowledgeIndexControlID,
			DesiredEpoch: 1,
			ActiveEpoch:  0,
			CreatedAt:    now,
			UpdatedAt:    now,
		}

		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&control).Error; err != nil {
			return err
		}

		bootstrap := model.KnowledgeIndexBootstrap{
			ID:        model.KnowledgeIndexControlID,
			Status:    "pending",
			CreatedAt: now,
		}

		return tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&bootstrap).Error
	})

	if err != nil {
		return fmt.Errorf("initialize knowledge index state: %w", err)
	}

	return nil
}
