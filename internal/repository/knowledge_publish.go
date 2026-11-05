package repository

import (
	"context"
	"time"

	"video-support-agent/internal/knowledgeworker"
	"video-support-agent/internal/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func (r *KnowledgeBuildRepository) Publish(ctx context.Context, verified *knowledgeworker.VerifiedGeneration) error {
	if verified == nil || verified.Snapshot == nil || verified.Snapshot.Prepared == nil {
		return knowledgeworker.ErrBuildState
	}
	claim := verified.Claim
	snapshot := verified.Snapshot
	if snapshot.SourceEpoch != claim.SourceEpoch {
		return knowledgeworker.ErrBuildState
	}

	rechecked, err := knowledgeworker.NewBuildSnapshot(ctx, snapshot.SourceEpoch, snapshot.Prepared, snapshot.Revisions)
	if err != nil {
		return err
	}
	if rechecked.ManifestSHA256 != snapshot.ManifestSHA256 || rechecked.ChunkStrategy != snapshot.ChunkStrategy {
		return knowledgeworker.ErrBuildState
	}

	obsolete := false
	err = r.transaction(ctx, func(tx *gorm.DB) error {
		control, err := lockBuildControl(tx)
		if err != nil {
			return err
		}

		var generation model.KnowledgeIndexGeneration
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", claim.GenerationID).Take(&generation).Error; err != nil {
			return err
		}

		target, err := generationTarget(generation)
		if err != nil ||
			generation.OwnerToken != claim.OwnerToken ||
			generation.SourceEpoch != claim.SourceEpoch ||
			generation.Attempt != claim.Attempt ||
			target.ManifestSHA256 != snapshot.ManifestSHA256 ||
			target.Identity != verified.Identity ||
			generation.DocumentCount != uint64(len(snapshot.Revisions)) ||
			generation.ChunkCount != uint64(len(snapshot.Prepared.Documents)) {
			return knowledgeworker.ErrBuildState
		}
		if !target.Empty && (target.ESIndex != claim.ESIndexName || target.MilvusCollection != claim.MilvusCollection) {
			return knowledgeworker.ErrBuildState
		}

		if generation.Status == model.KnowledgeGenerationPublished {
			var task model.KnowledgeIndexTask
			if err := tx.Where("source_epoch = ?", claim.SourceEpoch).
				Take(&task).Error; err != nil {
				return err
			}
			if generation.PublishedAt == nil ||
				task.Status != model.KnowledgeTaskCompleted ||
				task.GenerationID == nil ||
				*task.GenerationID != generation.ID ||
				control.ActiveEpoch < generation.SourceEpoch {
				return knowledgeworker.ErrBuildState
			}
			if control.ActiveEpoch == generation.SourceEpoch && (control.ActiveGenerationID == nil || *control.ActiveGenerationID != generation.ID) {
				return knowledgeworker.ErrBuildState
			}
			return nil
		}

		control, err = lockOwnedBuild(tx, claim)
		if err != nil {
			return err
		}
		now, err := buildDatabaseTime(tx)
		if err != nil {
			return err
		}

		if control.DesiredEpoch != claim.SourceEpoch {
			if claim.SourceEpoch > control.DesiredEpoch {
				return knowledgeworker.ErrBuildState
			}
			if err := retireOwnedBuild(tx, control, now); err != nil {
				return err
			}
			if err := ensureSourceEpochTask(tx, control, now); err != nil {
				return err
			}

			result := tx.Model(&model.KnowledgeIndexControl{}).
				Where("id = ? AND owner_token = ? AND lease_until > UTC_TIMESTAMP(3)",
					control.ID, claim.OwnerToken).
				Updates(map[string]any{
					"owner_token": nil,
					"lease_until": nil,
					"updated_at":  gorm.Expr("UTC_TIMESTAMP(3)"),
				})
			if err := requireOnePublicationRow(result); err != nil {
				return err
			}
			obsolete = true
			return nil
		}

		if err := applyPublishedDocuments(tx, snapshot, generation.ID, now); err != nil {
			return err
		}
		return finishPublication(tx, control, generation, claim, now)
	})
	if err != nil {
		return err
	}
	if obsolete {
		return knowledgeworker.ErrBuildObsolete
	}
	return nil
}

func applyPublishedDocuments(tx *gorm.DB, snapshot *knowledgeworker.BuildSnapshot, generationID string, now time.Time) error {
	revisions := snapshot.Revisions

	for start := 0; start < len(revisions); start += 100 {
		end := min(start+100, len(revisions))
		part := revisions[start:end]

		ids := make([]uint64, 0, len(part))
		for _, revision := range part {
			ids = append(ids, revision.DocumentID)
		}

		var documents []model.KnowledgeDocument
		if err := tx.Select("id", "publication_revision", "current_version_id").
			Where("id IN ?", ids).
			Order("id ASC").
			Clauses(clause.Locking{Strength: "UPDATE"}).
			Find(&documents).Error; err != nil {
			return err
		}
		if len(documents) != len(part) {
			return knowledgeworker.ErrBuildState
		}

		var invalidStates int64
		if err := tx.Model(&model.KnowledgeDocumentIndex{}).
			Where("document_id IN ?", ids).
			Where("expected_epoch > ? OR applied_epoch > ?", snapshot.SourceEpoch, snapshot.SourceEpoch).
			Count(&invalidStates).Error; err != nil {
			return err
		}
		if invalidStates != 0 {
			return knowledgeworker.ErrBuildState
		}

		states := make([]model.KnowledgeDocumentIndex, 0, len(part))
		for i, revision := range part {
			document := documents[i]
			if document.ID != revision.DocumentID ||
				document.PublicationRevision != revision.PublicationRevision ||
				!sameVersionID(document.CurrentVersionID, revision.CurrentVersionID) {
				return knowledgeworker.ErrBuildState
			}

			if revision.PublicationRevision == 0 {
				continue
			}

			states = append(states, model.KnowledgeDocumentIndex{
				DocumentID:                  revision.DocumentID,
				ExpectedPublicationRevision: revision.PublicationRevision,
				AppliedPublicationRevision:  revision.PublicationRevision,
				ExpectedEpoch:               snapshot.SourceEpoch,
				AppliedEpoch:                snapshot.SourceEpoch,
				AppliedGenerationID:         &generationID,
				Status:                      model.KnowledgeDocumentIndexReady,
				AppliedAt:                   &now,
				CreatedAt:                   now,
				UpdatedAt:                   now,
			})
		}

		if len(states) == 0 {
			continue
		}
		if err := tx.Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "document_id"}},
			DoUpdates: clause.AssignmentColumns([]string{
				"expected_publication_revision",
				"applied_publication_revision",
				"expected_epoch",
				"applied_epoch",
				"applied_generation_id",
				"status",
				"safe_error_code",
				"applied_at",
				"updated_at",
			}),
		}).Create(&states).Error; err != nil {
			return err
		}
	}
	return nil
}

func sameVersionID(left, right *uint64) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func finishPublication(tx *gorm.DB, control model.KnowledgeIndexControl, generation model.KnowledgeIndexGeneration, claim knowledgeworker.BuildClaim, now time.Time) error {
	result := tx.Model(&model.KnowledgeIndexGeneration{}).
		Where("id = ? AND status = ? AND owner_token = ?",
			generation.ID, model.KnowledgeGenerationVerified, claim.OwnerToken).
		Updates(map[string]any{
			"status":       model.KnowledgeGenerationPublished,
			"published_at": now,
			"updated_at":   now,
		})
	if err := requireOnePublicationRow(result); err != nil {
		return err
	}

	result = tx.Model(&model.KnowledgeIndexTask{}).
		Where("source_epoch = ? AND status = ? AND generation_id = ?",
			claim.SourceEpoch, model.KnowledgeTaskBuilding, generation.ID).
		Updates(map[string]any{
			"status":          model.KnowledgeTaskCompleted,
			"completed_at":    now,
			"safe_error_code": "",
			"updated_at":      now,
		})
	if err := requireOnePublicationRow(result); err != nil {
		return err
	}

	var bootstrap model.KnowledgeIndexBootstrap
	if err := tx.Where("id = ?", model.KnowledgeIndexControlID).
		Take(&bootstrap).Error; err != nil {
		return err
	}
	switch bootstrap.Status {
	case "scheduled":
		if err := requireOnePublicationRow(
			tx.Model(&model.KnowledgeIndexBootstrap{}).
				Where("id = ? AND status = ?", bootstrap.ID, "scheduled").
				Updates(map[string]any{
					"status":       "completed",
					"completed_at": now,
				}),
		); err != nil {
			return err
		}
	case "completed":
	default:
		return knowledgeworker.ErrBuildState
	}

	control.ActiveEpoch = generation.SourceEpoch
	control.ActiveGenerationID = &generation.ID
	if _, err := settleReadyEvents(tx, control, now); err != nil {
		return err
	}

	result = tx.Model(&model.KnowledgeIndexControl{}).
		Where(`id = ? AND owner_token = ? AND desired_epoch = ?
			AND active_epoch < ? AND lease_until > UTC_TIMESTAMP(3)`,
			control.ID, claim.OwnerToken, claim.SourceEpoch, claim.SourceEpoch).
		Updates(map[string]any{
			"active_epoch":         generation.SourceEpoch,
			"active_generation_id": generation.ID,
			"owner_token":          nil,
			"lease_until":          nil,
			"updated_at":           gorm.Expr("UTC_TIMESTAMP(3)"),
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return knowledgeworker.ErrBuildLeaseLost
	}
	return nil
}
