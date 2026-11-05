package repository

import (
	"context"
	"video-support-agent/internal/knowledgefailure"

	"video-support-agent/internal/knowledgeworker"
	"video-support-agent/internal/model"

	"gorm.io/gorm"
)

var _ knowledgeworker.RuntimeStore = (*KnowledgeBuildRepository)(nil)

func (r *KnowledgeBuildRepository) HoldFailure(ctx context.Context, claim knowledgeworker.BuildClaim, code string) error {
	if _, known := knowledgefailure.Classify(code); !known {
		return knowledgeworker.ErrBuildState
	}
	if claim.Attempt < 1 || claim.Attempt > knowledgefailure.MaxAttempts {
		return knowledgeworker.ErrBuildState
	}

	return r.transaction(ctx, func(tx *gorm.DB) error {
		control, err := lockOwnedBuild(tx, claim)
		if err != nil {
			return err
		}
		if control.DesiredEpoch != claim.SourceEpoch {
			return knowledgeworker.ErrBuildObsolete
		}

		now, err := buildDatabaseTime(tx)
		if err != nil {
			return err
		}

		result := tx.Model(&model.KnowledgeIndexGeneration{}).
			Where("id = ? AND owner_token = ? AND status IN ?",
				claim.GenerationID, claim.OwnerToken,
				[]string{
					model.KnowledgeGenerationBuilding,
					model.KnowledgeGenerationVerified,
				}).
			Updates(map[string]any{
				"status":          model.KnowledgeGenerationAbandoned,
				"safe_error_code": code,
				"updated_at":      now,
			})
		if err := requireOnePublicationRow(result); err != nil {
			return err
		}

		result = tx.Model(&model.KnowledgeIndexTask{}).
			Where(`source_epoch = ? AND status = ? AND generation_id = ?
				AND failed_attempts = ?`,
				claim.SourceEpoch, model.KnowledgeTaskBuilding,
				claim.GenerationID, claim.Attempt-1).
			Updates(map[string]any{
				"status":          model.KnowledgeTaskBlocked,
				"failed_attempts": claim.Attempt,
				"safe_error_code": code,
				"updated_at":      now,
			})
		if err := requireOnePublicationRow(result); err != nil {
			return err
		}

		result = tx.Model(&model.KnowledgeIndexControl{}).
			Where(`id = ? AND owner_token = ? AND desired_epoch = ?
				AND lease_until > UTC_TIMESTAMP(3)`,
				control.ID, claim.OwnerToken, claim.SourceEpoch).
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
