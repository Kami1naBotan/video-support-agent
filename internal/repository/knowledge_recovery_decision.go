package repository

import (
	"context"
	"crypto/rand"
	"errors"
	"math/big"
	"time"

	"video-support-agent/internal/knowledgefailure"
	"video-support-agent/internal/knowledgeworker"
	"video-support-agent/internal/model"

	"gorm.io/gorm"
)

var _ knowledgeworker.RecoveryStore = (*KnowledgeBuildRepository)(nil)

func newBuildRecovery(generation model.KnowledgeIndexGeneration, code string, now time.Time) (model.KnowledgeBuildRecovery, error) {
	var zero model.KnowledgeBuildRecovery

	value, err := rand.Int(rand.Reader, big.NewInt(201))
	if err != nil {
		return zero, err
	}
	jitter := uint32(value.Uint64())

	decision, err := knowledgefailure.Plan(code, generation.Attempt, now, jitter)
	if err != nil {
		return zero, knowledgeworker.ErrBuildState
	}

	row := model.KnowledgeBuildRecovery{
		GenerationID:   generation.ID,
		SourceEpoch:    generation.SourceEpoch,
		FailedAttempt:  generation.Attempt,
		FailureCode:    decision.FailureCode,
		ReasonCode:     decision.ReasonCode,
		Action:         string(decision.Action),
		NextAttempt:    decision.NextAttempt,
		JitterPermille: jitter,
		DecidedAt:      now,
		Status:         model.KnowledgeRecoveryPending,
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	if decision.Action == knowledgefailure.ActionRetry {
		at := decision.NotBefore
		row.NotBefore = &at
	}

	return row, nil
}

func validateBuildRecovery(row model.KnowledgeBuildRecovery) error {
	if row.GenerationID == "" || row.SourceEpoch == 0 {
		return knowledgeworker.ErrBuildState
	}

	decision, err := knowledgefailure.Plan(row.FailureCode, row.FailedAttempt, row.DecidedAt, row.JitterPermille)
	if err != nil || row.Action != string(decision.Action) || row.ReasonCode != decision.ReasonCode || row.NextAttempt != decision.NextAttempt {
		return knowledgeworker.ErrBuildState
	}

	switch decision.Action {
	case knowledgefailure.ActionRetry:
		if row.NotBefore == nil || !row.NotBefore.Equal(decision.NotBefore) {
			return knowledgeworker.ErrBuildState
		}
	case knowledgefailure.ActionDeadLetter:
		if row.NotBefore != nil {
			return knowledgeworker.ErrBuildState
		}
	default:
		return knowledgeworker.ErrBuildState
	}

	switch row.Status {
	case model.KnowledgeRecoveryPending:
		if row.CompletedAt != nil {
			return knowledgeworker.ErrBuildState
		}
	case model.KnowledgeRecoveryCompleted:
		if row.CompletedAt == nil || row.CompletedAt.IsZero() {
			return knowledgeworker.ErrBuildState
		}
	default:
		return knowledgeworker.ErrBuildState
	}

	return nil
}

func recordedBuildFailure(tx *gorm.DB, claim knowledgeworker.BuildClaim, code string) (bool, error) {
	var row model.KnowledgeBuildRecovery

	err := tx.Where("generation_id = ?", claim.GenerationID).
		Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	if row.SourceEpoch != claim.SourceEpoch || row.FailedAttempt != claim.Attempt || row.FailureCode != code {
		return false, knowledgeworker.ErrBuildState
	}
	if err := validateBuildRecovery(row); err != nil {
		return false, err
	}

	var generation model.KnowledgeIndexGeneration
	if err := tx.Where("id = ?", claim.GenerationID).Take(&generation).Error; err != nil {
		return false, err
	}

	if generation.OwnerToken != claim.OwnerToken ||
		generation.SourceEpoch != claim.SourceEpoch ||
		generation.Attempt != claim.Attempt ||
		generation.Status != model.KnowledgeGenerationAbandoned ||
		generation.SafeErrorCode != code {
		return false, knowledgeworker.ErrBuildState
	}

	return true, nil
}

func (r *KnowledgeBuildRepository) RecordBuildFailure(ctx context.Context, claim knowledgeworker.BuildClaim, code string) error {
	if _, known := knowledgefailure.Classify(code); !known {
		return knowledgeworker.ErrBuildState
	}
	if claim.GenerationID == "" ||
		claim.OwnerToken == "" ||
		claim.SourceEpoch == 0 ||
		claim.Attempt < 1 ||
		claim.Attempt > knowledgefailure.MaxAttempts {
		return knowledgeworker.ErrBuildState
	}

	return r.transaction(ctx, func(tx *gorm.DB) error {
		control, err := lockBuildControl(tx)
		if err != nil {
			return err
		}

		recorded, err := recordedBuildFailure(tx, claim, code)
		if err != nil {
			return err
		}
		if recorded {
			return nil
		}

		control, err = lockOwnedBuild(tx, claim)
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

		generation := model.KnowledgeIndexGeneration{
			ID:          claim.GenerationID,
			SourceEpoch: claim.SourceEpoch,
			Attempt:     claim.Attempt,
		}
		recovery, err := newBuildRecovery(generation, code, now)
		if err != nil {
			return err
		}
		if err := tx.Create(&recovery).Error; err != nil {
			return err
		}

		result := tx.Model(&model.KnowledgeIndexGeneration{}).
			Where(
				"id = ? AND owner_token = ? AND status IN ?",
				claim.GenerationID,
				claim.OwnerToken,
				[]string{
					model.KnowledgeGenerationBuilding,
					model.KnowledgeGenerationVerified,
				},
			).
			Updates(map[string]any{
				"status":          model.KnowledgeGenerationAbandoned,
				"safe_error_code": code,
				"updated_at":      now,
			})
		if err := requireOnePublicationRow(result); err != nil {
			return err
		}

		result = tx.Model(&model.KnowledgeIndexTask{}).
			Where(
				`source_epoch = ? AND status = ?
				 AND generation_id = ? AND failed_attempts = ?`,
				claim.SourceEpoch,
				model.KnowledgeTaskBuilding,
				claim.GenerationID,
				claim.Attempt-1,
			).
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
			Where(
				`id = ? AND owner_token = ? AND desired_epoch = ?
				 AND lease_until > UTC_TIMESTAMP(3)`,
				control.ID,
				claim.OwnerToken,
				claim.SourceEpoch,
			).
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

func (r *KnowledgeBuildRepository) AdoptBlockedFailure(ctx context.Context) (bool, error) {
	adopted := false

	err := r.transaction(ctx, func(tx *gorm.DB) error {
		control, err := lockBuildControl(tx)
		if err != nil {
			return err
		}

		if control.OwnerToken != nil {
			return nil
		}

		var task model.KnowledgeIndexTask
		err = tx.Table("knowledge_index_tasks AS t").
			Select("t.*").
			Where("t.status IN ?", []string{
				model.KnowledgeTaskBlocked,
				model.KnowledgeTaskObsolete,
			}).
			Where("t.generation_id IS NOT NULL").
			Where("t.safe_error_code IN ?", []string{
				knowledgefailure.CodeProjectionConflict,
				knowledgefailure.CodeProjectionLimit,
				knowledgefailure.CodeDualInputInvalid,
				knowledgefailure.CodeBuildTimeout,
				knowledgefailure.CodeDualExternalFailed,
			}).
			Where(`NOT EXISTS (
				SELECT 1
				FROM knowledge_build_recoveries AS r
				WHERE r.generation_id = t.generation_id
			)`).
			Order("t.source_epoch ASC").
			Take(&task).Error

		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return err
		}

		if task.GenerationID == nil ||
			task.SourceEpoch == 0 ||
			task.SourceEpoch > control.DesiredEpoch ||
			task.FailedAttempts < 1 ||
			task.FailedAttempts > knowledgefailure.MaxAttempts {
			return knowledgeworker.ErrBuildState
		}

		var generation model.KnowledgeIndexGeneration
		if err := tx.Where("id = ?", *task.GenerationID).Take(&generation).Error; err != nil {
			return err
		}

		if generation.Status != model.KnowledgeGenerationAbandoned ||
			generation.SourceEpoch != task.SourceEpoch ||
			generation.Attempt != task.FailedAttempts ||
			generation.SafeErrorCode != task.SafeErrorCode {
			return knowledgeworker.ErrBuildState
		}
		if control.ActiveGenerationID != nil && *control.ActiveGenerationID == generation.ID {
			return knowledgeworker.ErrBuildState
		}

		now, err := buildDatabaseTime(tx)
		if err != nil {
			return err
		}

		recovery, err := newBuildRecovery(generation, task.SafeErrorCode, now)
		if err != nil {
			return err
		}
		if err := tx.Create(&recovery).Error; err != nil {
			return err
		}

		adopted = true
		return nil
	})
	if err != nil {
		return false, err
	}

	return adopted, nil
}
