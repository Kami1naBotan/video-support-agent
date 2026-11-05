package repository

import (
	"context"
	"time"
	"video-support-agent/internal/knowledgeworker"
	"video-support-agent/internal/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func lockOwnedBuild(tx *gorm.DB, claim knowledgeworker.BuildClaim) (model.KnowledgeIndexControl, error) {
	control, err := lockBuildControl(tx)
	if err != nil {
		return control, err
	}
	if control.OwnerToken == nil || *control.OwnerToken != claim.OwnerToken || claim.GenerationID == "" || claim.SourceEpoch == 0 {
		return control, knowledgeworker.ErrBuildLeaseLost
	}

	var generation model.KnowledgeIndexGeneration
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ?", claim.GenerationID).
		Take(&generation).Error; err != nil {
		return control, err
	}
	if generation.OwnerToken != claim.OwnerToken ||
		generation.SourceEpoch != claim.SourceEpoch ||
		generation.Attempt != claim.Attempt ||
		(generation.Status != model.KnowledgeGenerationBuilding && generation.Status != model.KnowledgeGenerationVerified) {
		return control, knowledgeworker.ErrBuildLeaseLost
	}
	if control.ActiveGenerationID != nil && *control.ActiveGenerationID == generation.ID {
		return control, knowledgeworker.ErrBuildState
	}

	var task model.KnowledgeIndexTask
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("source_epoch = ?", claim.SourceEpoch).Take(&task).Error; err != nil {
		return control, err
	}
	if task.Status != model.KnowledgeTaskBuilding || task.GenerationID == nil || *task.GenerationID != claim.GenerationID {
		return control, knowledgeworker.ErrBuildLeaseLost
	}

	now, err := buildDatabaseTime(tx)
	if err != nil {
		return control, err
	}
	if !control.LeaseUntil.After(now) {
		return control, knowledgeworker.ErrBuildLeaseLost
	}
	return control, nil
}

func (r *KnowledgeBuildRepository) Renew(ctx context.Context, claim knowledgeworker.BuildClaim) error {
	return r.transaction(ctx, func(tx *gorm.DB) error {
		control, err := lockOwnedBuild(tx, claim)
		if err != nil {
			return err
		}
		if control.DesiredEpoch != claim.SourceEpoch {
			return knowledgeworker.ErrBuildObsolete
		}

		result := tx.Model(&model.KnowledgeIndexControl{}).
			Where("id = ? AND owner_token = ? AND lease_until > UTC_TIMESTAMP(3)", control.ID, claim.OwnerToken).
			Updates(map[string]any{
				"lease_until": gorm.Expr(
					"TIMESTAMPADD(SECOND, ?, UTC_TIMESTAMP(3))",
					int64(knowledgeworker.BuildLeaseDuration/time.Second),
				),
				"updated_at": gorm.Expr("UTC_TIMESTAMP(3)"),
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return knowledgeworker.ErrBuildLeaseLost
		}
		return nil
	})
}

func retireOwnedBuild(tx *gorm.DB, control model.KnowledgeIndexControl, now time.Time) error {
	if control.OwnerToken == nil {
		return knowledgeworker.ErrBuildState
	}

	var generation model.KnowledgeIndexGeneration
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("owner_token = ?", *control.OwnerToken).
		Take(&generation).Error; err != nil {
		return err
	}
	if generation.Status != model.KnowledgeGenerationBuilding && generation.Status != model.KnowledgeGenerationVerified {
		return knowledgeworker.ErrBuildState
	}
	if control.ActiveGenerationID != nil && *control.ActiveGenerationID == generation.ID {
		return knowledgeworker.ErrBuildState
	}

	var task model.KnowledgeIndexTask
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("source_epoch = ?", generation.SourceEpoch).
		Take(&task).Error; err != nil {
		return err
	}
	if task.Status != model.KnowledgeTaskBuilding ||
		task.GenerationID == nil ||
		*task.GenerationID != generation.ID ||
		task.SourceEpoch > control.DesiredEpoch {
		return knowledgeworker.ErrBuildState
	}

	generationStatus := model.KnowledgeGenerationAbandoned
	taskStatus := model.KnowledgeTaskPending
	var completedAt *time.Time

	if task.SourceEpoch < control.DesiredEpoch {
		generationStatus = model.KnowledgeGenerationObsolete
		taskStatus = model.KnowledgeTaskObsolete
		completedAt = &now
	}

	if err := tx.Model(&model.KnowledgeIndexGeneration{}).
		Where("id = ?", generation.ID).
		Updates(map[string]any{
			"status":     generationStatus,
			"updated_at": now,
		}).Error; err != nil {
		return err
	}

	return tx.Model(&model.KnowledgeIndexTask{}).
		Where("source_epoch = ?", task.SourceEpoch).
		Updates(map[string]any{
			"status":          taskStatus,
			"generation_id":   nil,
			"not_before":      now,
			"safe_error_code": "",
			"completed_at":    completedAt,
			"updated_at":      now,
		}).Error
}

func (r *KnowledgeBuildRepository) Release(ctx context.Context, claim knowledgeworker.BuildClaim) error {
	return r.transaction(ctx, func(tx *gorm.DB) error {
		control, err := lockOwnedBuild(tx, claim)
		if err != nil {
			return err
		}
		now, err := buildDatabaseTime(tx)
		if err != nil {
			return err
		}

		if err := retireOwnedBuild(tx, control, now); err != nil {
			return err
		}
		if err := ensureSourceEpochTask(tx, control, now); err != nil {
			return err
		}

		result := tx.Model(&model.KnowledgeIndexControl{}).
			Where("id = ? AND owner_token = ? AND lease_until > UTC_TIMESTAMP(3)", control.ID, claim.OwnerToken).
			Updates(map[string]any{
				"owner_token": nil,
				"lease_until": nil,
				"updated_at":  gorm.Expr("UTC_TIMESTAMP(3)"),
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return knowledgeworker.ErrBuildLeaseLost
		}
		return nil
	})
}
