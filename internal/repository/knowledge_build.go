package repository

import (
	"context"
	"errors"
	"strings"
	"time"
	"video-support-agent/internal/knowledgefailure"

	"video-support-agent/internal/knowledgeevent"
	"video-support-agent/internal/knowledgeworker"
	"video-support-agent/internal/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

type KnowledgeBuildRepository struct {
	db              *gorm.DB
	generationLimit uint64
}

var _ knowledgeworker.BuildStore = (*KnowledgeBuildRepository)(nil)

func NewKnowledgeBuildRepository(db *gorm.DB, generationLimit uint64) (*KnowledgeBuildRepository, error) {
	if db == nil || generationLimit == 0 {
		return nil, knowledgeworker.ErrBuildConfig
	}
	return &KnowledgeBuildRepository{
		db:              db,
		generationLimit: generationLimit,
	}, nil
}

func (r *KnowledgeBuildRepository) transaction(ctx context.Context, fn func(*gorm.DB) error) error {
	if r == nil || r.db == nil {
		return knowledgeworker.ErrBuildConfig
	}

	bounded, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	db := r.db.Session(&gorm.Session{Logger: logger.Default.LogMode(logger.Silent)}).WithContext(bounded)

	err := db.Transaction(fn)

	if ctx.Err() != nil {
		return ctx.Err()
	}
	if bounded.Err() != nil {
		return knowledgeworker.ErrBuildStore
	}
	if err == nil {
		return nil
	}

	for _, known := range []error{
		knowledgeworker.ErrBuildConfig,
		knowledgeworker.ErrBuildState,
		knowledgeworker.ErrBuildLeaseLost,
		knowledgeworker.ErrBuildObsolete,
	} {
		if errors.Is(err, known) {
			return known
		}
	}
	return knowledgeworker.ErrBuildStore
}

func lockBuildControl(tx *gorm.DB) (model.KnowledgeIndexControl, error) {
	var control model.KnowledgeIndexControl
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ?", model.KnowledgeIndexControlID).
		Take(&control).Error
	if err != nil {
		return control, err
	}

	if control.DesiredEpoch == 0 ||
		control.ActiveEpoch > control.DesiredEpoch ||
		(control.ActiveEpoch == 0 && control.ActiveGenerationID != nil) ||
		(control.ActiveEpoch > 0 && control.ActiveGenerationID == nil) ||
		(control.OwnerToken == nil) != (control.LeaseUntil == nil) {
		return control, knowledgeworker.ErrBuildState
	}
	return control, nil
}

func buildDatabaseTime(tx *gorm.DB) (time.Time, error) {
	var row struct {
		Now time.Time
	}
	if err := tx.Raw("SELECT UTC_TIMESTAMP(3) AS now").
		Scan(&row).Error; err != nil {
		return time.Time{}, err
	}
	if row.Now.IsZero() {
		return time.Time{}, knowledgeworker.ErrBuildState
	}
	return row.Now.UTC(), nil
}

func (r *KnowledgeBuildRepository) ClaimLatest(ctx context.Context) (*knowledgeworker.BuildClaim, error) {
	var claimed *knowledgeworker.BuildClaim
	var decisionErr error

	err := r.transaction(ctx, func(tx *gorm.DB) error {
		control, err := lockBuildControl(tx)
		if err != nil {
			return err
		}
		now, err := buildDatabaseTime(tx)
		if err != nil {
			return err
		}

		if control.GenerationLimit == 0 {
			if err := tx.Model(&model.KnowledgeIndexControl{}).
				Where("id = ?", control.ID).
				Update("generation_limit", r.generationLimit).Error; err != nil {
				return err
			}
			control.GenerationLimit = r.generationLimit
		}
		if control.GenerationLimit != r.generationLimit {
			return knowledgeworker.ErrBuildConfig
		}

		var pendingRecovery model.KnowledgeBuildRecovery
		recoveryErr := tx.
			Select("generation_id").Where("status = ?", model.KnowledgeRecoveryPending).Take(&pendingRecovery).Error
		if recoveryErr == nil {
			return nil
		}
		if !errors.Is(recoveryErr, gorm.ErrRecordNotFound) {
			return recoveryErr
		}

		if control.OwnerToken != nil {
			if control.LeaseUntil.After(now) {
				return nil
			}
			if err := retireOwnedBuild(tx, control, now); err != nil {
				return err
			}
			if err := tx.Model(&model.KnowledgeIndexControl{}).
				Where("id = ?", control.ID).
				Updates(map[string]any{
					"owner_token": nil,
					"lease_until": nil,
					"updated_at":  now,
				}).Error; err != nil {
				return err
			}
		}

		if err := scheduleBootstrap(tx, control, now); err != nil {
			return err
		}

		if err := tx.Model(&model.KnowledgeIndexTask{}).
			Where("source_epoch < ? AND status IN ?", control.DesiredEpoch,
				[]string{
					model.KnowledgeTaskPending,
					model.KnowledgeTaskRetryWait,
					model.KnowledgeTaskBlocked,
				}).
			Updates(map[string]any{
				"status":       model.KnowledgeTaskObsolete,
				"completed_at": now,
				"updated_at":   now,
			}).Error; err != nil {
			return err
		}

		if control.ActiveEpoch == control.DesiredEpoch {
			return nil
		}

		var task model.KnowledgeIndexTask
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("source_epoch = ?", control.DesiredEpoch).
			Take(&task).Error; err != nil {
			return err
		}

		switch task.Status {
		case model.KnowledgeTaskPending, model.KnowledgeTaskRetryWait:
		case model.KnowledgeTaskBlocked:
			if task.SafeErrorCode != "resource_capacity" {
				decisionErr = knowledgeworker.ErrBuildExhausted
				return nil
			}
		default:
			return knowledgeworker.ErrBuildState
		}

		if task.NotBefore.After(now) {
			return nil
		}
		waiting, err := waitingForRecoveryDelivery(tx, task.SourceEpoch, now)
		if err != nil {
			return err
		}
		if waiting {
			return nil
		}
		if task.FailedAttempts >= knowledgefailure.MaxAttempts {
			decisionErr = knowledgeworker.ErrBuildExhausted
			return blockBuildTask(
				tx,
				task.SourceEpoch,
				knowledgefailure.ReasonAttemptsExhausted,
				now,
			)
		}

		var generations int64
		if err := tx.Model(&model.KnowledgeIndexGeneration{}).
			Count(&generations).Error; err != nil {
			return err
		}
		if generations < 0 {
			return knowledgeworker.ErrBuildState
		}
		if uint64(generations) >= control.GenerationLimit {
			decisionErr = knowledgeworker.ErrBuildCapacity
			return blockBuildTask(tx, task.SourceEpoch, "resource_capacity", now)
		}

		claim, err := createBuildGeneration(tx, task, now)
		if err != nil {
			return err
		}
		if err := bindBuildEvents(tx, *claim, now); err != nil {
			return err
		}
		claimed = claim
		return nil
	})
	if err != nil {
		return nil, err
	}
	if decisionErr != nil {
		return nil, decisionErr
	}
	return claimed, nil
}

func scheduleBootstrap(tx *gorm.DB, control model.KnowledgeIndexControl, now time.Time) error {
	var bootstrap model.KnowledgeIndexBootstrap
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ?", model.KnowledgeIndexControlID).
		Take(&bootstrap).Error; err != nil {
		return err
	}

	switch bootstrap.Status {
	case "pending", "scheduled", "completed":
	default:
		return knowledgeworker.ErrBuildState
	}

	if err := ensureSourceEpochTask(tx, control, now); err != nil {
		return err
	}
	if bootstrap.Status != "pending" {
		return nil
	}

	return tx.Model(&model.KnowledgeIndexBootstrap{}).
		Where("id = ? AND status = ?", bootstrap.ID, "pending").
		Update("status", "scheduled").Error
}

func blockBuildTask(tx *gorm.DB, epoch uint64, code string, now time.Time) error {
	return tx.Model(&model.KnowledgeIndexTask{}).
		Where("source_epoch = ?", epoch).
		Updates(map[string]any{
			"status":          model.KnowledgeTaskBlocked,
			"safe_error_code": code,
			"updated_at":      now,
		}).Error
}

func createBuildGeneration(tx *gorm.DB, task model.KnowledgeIndexTask, now time.Time) (*knowledgeworker.BuildClaim, error) {
	generationID, err := knowledgeevent.NewID()
	if err != nil {
		return nil, err
	}
	token, err := knowledgeevent.NewID()
	if err != nil {
		return nil, err
	}

	suffix := strings.ReplaceAll(generationID, "-", "")
	esName := "knowledge_g_" + suffix
	milvusName := "rule_chunks_vec_v1_" + suffix
	attempt := task.FailedAttempts + 1

	generation := model.KnowledgeIndexGeneration{
		ID:               generationID,
		SourceEpoch:      task.SourceEpoch,
		OwnerToken:       token,
		Attempt:          attempt,
		Status:           model.KnowledgeGenerationBuilding,
		ESIndexName:      &esName,
		MilvusCollection: &milvusName,
		ESStatus:         model.KnowledgeTargetPending,
		MilvusStatus:     model.KnowledgeTargetPending,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	if err := tx.Create(&generation).Error; err != nil {
		return nil, err
	}

	if err := tx.Model(&model.KnowledgeIndexTask{}).
		Where("source_epoch = ?", task.SourceEpoch).
		Updates(map[string]any{
			"status":          model.KnowledgeTaskBuilding,
			"generation_id":   generationID,
			"safe_error_code": "",
			"completed_at":    nil,
			"updated_at":      now,
		}).Error; err != nil {
		return nil, err
	}

	if err := tx.Model(&model.KnowledgeIndexControl{}).
		Where("id = ?", model.KnowledgeIndexControlID).
		Updates(map[string]any{
			"owner_token": token,
			"lease_until": gorm.Expr(
				"TIMESTAMPADD(SECOND, ?, UTC_TIMESTAMP(3))",
				int64(knowledgeworker.BuildLeaseDuration/time.Second),
			),
			"updated_at": gorm.Expr("UTC_TIMESTAMP(3)"),
		}).Error; err != nil {
		return nil, err
	}

	return &knowledgeworker.BuildClaim{
		GenerationID:     generationID,
		SourceEpoch:      task.SourceEpoch,
		OwnerToken:       token,
		Attempt:          attempt,
		ESIndexName:      esName,
		MilvusCollection: milvusName,
	}, nil
}
