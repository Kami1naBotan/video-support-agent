package repository

import (
	"context"

	"video-support-agent/internal/embedding"
	"video-support-agent/internal/knowledgeworker"
	"video-support-agent/internal/model"

	"gorm.io/gorm"
)

var _ knowledgeworker.GenerationStore = (*KnowledgeBuildRepository)(nil)

func writableGeneration(tx *gorm.DB, claim knowledgeworker.BuildClaim) (model.KnowledgeIndexGeneration, error) {
	var generation model.KnowledgeIndexGeneration

	control, err := lockOwnedBuild(tx, claim)
	if err != nil {
		return generation, err
	}
	if control.DesiredEpoch != claim.SourceEpoch {
		return generation, knowledgeworker.ErrBuildObsolete
	}

	if err := tx.Where("id = ?", claim.GenerationID).
		Take(&generation).Error; err != nil {
		return generation, err
	}
	if generation.Status != model.KnowledgeGenerationBuilding {
		return generation, knowledgeworker.ErrBuildState
	}
	return generation, nil
}

func updateGeneration(tx *gorm.DB, claim knowledgeworker.BuildClaim, phase string, updates map[string]any) error {
	updates["updated_at"] = gorm.Expr("UTC_TIMESTAMP(3)")

	result := tx.Model(&model.KnowledgeIndexGeneration{}).
		Where("id = ? AND owner_token = ? AND status = ? AND build_phase = ?",
			claim.GenerationID, claim.OwnerToken,
			model.KnowledgeGenerationBuilding, phase).
		Where(`EXISTS (
			SELECT 1 FROM knowledge_index_controls
			WHERE id = ? AND owner_token = ?
			  AND desired_epoch = ? AND lease_until > UTC_TIMESTAMP(3)
		)`, model.KnowledgeIndexControlID, claim.OwnerToken, claim.SourceEpoch).
		Updates(updates)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return knowledgeworker.ErrBuildLeaseLost
	}
	return nil
}

func (r *KnowledgeBuildRepository) StartGeneration(ctx context.Context, claim knowledgeworker.BuildClaim, snapshot *knowledgeworker.BuildSnapshot, identity embedding.ModelIdentity) error {
	if snapshot == nil || snapshot.Prepared == nil || snapshot.SourceEpoch != claim.SourceEpoch || snapshot.ManifestSHA256 == "" {
		return knowledgeworker.ErrBuildState
	}

	return r.transaction(ctx, func(tx *gorm.DB) error {
		generation, err := writableGeneration(tx, claim)
		if err != nil {
			return err
		}
		if generation.Empty != nil || generation.ManifestSHA256 != nil ||
			generation.BuildPhase != "" ||
			generation.ESStatus != model.KnowledgeTargetPending ||
			generation.MilvusStatus != model.KnowledgeTargetPending ||
			generation.ESIndexName == nil ||
			*generation.ESIndexName != claim.ESIndexName ||
			generation.MilvusCollection == nil ||
			*generation.MilvusCollection != claim.MilvusCollection {
			return knowledgeworker.ErrBuildState
		}

		empty := len(snapshot.Prepared.Documents) == 0
		updates := map[string]any{
			"build_phase":              knowledgeworker.PhasePrepared,
			"empty":                    empty,
			"source_database":          snapshot.Prepared.SourceDatabase,
			"source_server_uuid":       snapshot.Prepared.SourceServerUUID,
			"snapshot_at":              snapshot.Prepared.CapturedAt,
			"manifest_sha256":          snapshot.ManifestSHA256,
			"document_count":           uint64(len(snapshot.Revisions)),
			"chunk_count":              uint64(len(snapshot.Prepared.Documents)),
			"embedding_model":          identity.Model,
			"embedding_digest":         identity.Digest,
			"embedding_dimension":      uint32(identity.Dimensions),
			"embedding_server_version": identity.ServerVersion,
			"embedding_context_length": uint32(identity.ContextLength),
			"embedding_input_policy":   identity.InputPolicy,
			"normalization":            knowledgeworker.VectorNormalization,
			"chunk_strategy":           snapshot.ChunkStrategy,
		}
		if empty {
			updates["es_index_name"] = nil
			updates["milvus_collection"] = nil
		}
		return updateGeneration(tx, claim, "", updates)
	})
}

func nextBuildPhase(from, to string) bool {
	switch from {
	case knowledgeworker.PhasePrepared:
		return to == knowledgeworker.PhaseESCreate
	case knowledgeworker.PhaseESCreate:
		return to == knowledgeworker.PhaseESBulk
	case knowledgeworker.PhaseESBulk:
		return to == knowledgeworker.PhaseESVerify
	case knowledgeworker.PhaseESVerify:
		return to == knowledgeworker.PhaseMilvusBuild
	case knowledgeworker.PhaseMilvusBuild:
		return to == knowledgeworker.PhaseMilvusVerify
	case knowledgeworker.PhaseMilvusVerify:
		return to == knowledgeworker.PhaseSealed
	default:
		return false
	}
}

func (r *KnowledgeBuildRepository) AdvanceGeneration(ctx context.Context, claim knowledgeworker.BuildClaim, manifest string, from string, to string) error {
	return r.transaction(ctx, func(tx *gorm.DB) error {
		generation, err := writableGeneration(tx, claim)
		if err != nil {
			return err
		}
		if generation.Empty == nil ||
			generation.ManifestSHA256 == nil ||
			*generation.ManifestSHA256 != manifest ||
			generation.BuildPhase != from {
			return knowledgeworker.ErrBuildState
		}

		empty := *generation.Empty
		if empty {
			if from != knowledgeworker.PhasePrepared ||
				to != knowledgeworker.PhaseSealed ||
				generation.ESIndexName != nil ||
				generation.MilvusCollection != nil {
				return knowledgeworker.ErrBuildState
			}
		} else {
			if !nextBuildPhase(from, to) ||
				generation.ESIndexName == nil ||
				*generation.ESIndexName != claim.ESIndexName ||
				generation.MilvusCollection == nil ||
				*generation.MilvusCollection != claim.MilvusCollection {
				return knowledgeworker.ErrBuildState
			}
		}

		updates := map[string]any{
			"build_phase": to,
		}
		switch to {
		case knowledgeworker.PhaseESVerify:
			updates["es_status"] = model.KnowledgeTargetBuilt

		case knowledgeworker.PhaseMilvusBuild:
			if generation.ESStatus != model.KnowledgeTargetBuilt {
				return knowledgeworker.ErrBuildState
			}
			updates["es_status"] = model.KnowledgeTargetVerified
			updates["es_manifest_sha256"] = manifest

		case knowledgeworker.PhaseMilvusVerify:
			updates["milvus_status"] = model.KnowledgeTargetBuilt

		case knowledgeworker.PhaseSealed:
			if !empty {
				if generation.ESStatus != model.KnowledgeTargetVerified ||
					generation.ESManifestSHA256 == nil ||
					*generation.ESManifestSHA256 != manifest ||
					generation.MilvusStatus != model.KnowledgeTargetBuilt {
					return knowledgeworker.ErrBuildState
				}
			}
			updates["es_status"] = model.KnowledgeTargetVerified
			updates["milvus_status"] = model.KnowledgeTargetVerified
			updates["es_manifest_sha256"] = manifest
			updates["milvus_manifest_sha256"] = manifest
			updates["status"] = model.KnowledgeGenerationVerified
			updates["verified_at"] = gorm.Expr("UTC_TIMESTAMP(3)")
		}

		return updateGeneration(tx, claim, from, updates)
	})
}
