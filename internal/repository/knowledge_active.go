package repository

import (
	"context"
	"errors"
	"time"
	"video-support-agent/internal/embedding"
	"video-support-agent/internal/generationquery"
	"video-support-agent/internal/knowledgechunk"
	"video-support-agent/internal/knowledgeworker"
	"video-support-agent/internal/model"
	"video-support-agent/internal/retrieval"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func generationTarget(g model.KnowledgeIndexGeneration) (generationquery.Target, error) {
	var zero generationquery.Target

	if (g.Status != model.KnowledgeGenerationVerified &&
		g.Status != model.KnowledgeGenerationPublished) ||
		g.BuildPhase != knowledgeworker.PhaseSealed ||
		g.Empty == nil || g.ManifestSHA256 == nil ||
		g.ESManifestSHA256 == nil || g.MilvusManifestSHA256 == nil ||
		*g.ESManifestSHA256 != *g.ManifestSHA256 ||
		*g.MilvusManifestSHA256 != *g.ManifestSHA256 ||
		g.ESStatus != model.KnowledgeTargetVerified ||
		g.MilvusStatus != model.KnowledgeTargetVerified ||
		g.VerifiedAt == nil || g.VerifiedAt.IsZero() ||
		g.SourceDatabase == nil || *g.SourceDatabase == "" ||
		g.SourceServerUUID == nil || *g.SourceServerUUID == "" ||
		g.SnapshotAt == nil || g.SnapshotAt.IsZero() ||
		g.EmbeddingModel == nil || g.EmbeddingDigest == nil ||
		g.EmbeddingDimension == nil || g.EmbeddingServerVersion == nil ||
		g.EmbeddingContextLength == nil || g.EmbeddingInputPolicy == nil ||
		g.Normalization == nil ||
		*g.Normalization != knowledgeworker.VectorNormalization ||
		g.ChunkStrategy == nil || *g.ChunkStrategy != knowledgechunk.Strategy ||
		*g.Empty != (g.ChunkCount == 0) {
		return zero, generationquery.ErrTarget
	}

	target := generationquery.Target{
		GenerationID:   g.ID,
		Epoch:          g.SourceEpoch,
		Empty:          *g.Empty,
		ManifestSHA256: *g.ManifestSHA256,
		Identity: embedding.ModelIdentity{
			Model:         *g.EmbeddingModel,
			Digest:        *g.EmbeddingDigest,
			ServerVersion: *g.EmbeddingServerVersion,
			Dimensions:    int(*g.EmbeddingDimension),
			ContextLength: int(*g.EmbeddingContextLength),
			InputPolicy:   *g.EmbeddingInputPolicy,
		},
	}

	if target.Empty {
		if g.ESIndexName != nil || g.MilvusCollection != nil {
			return zero, generationquery.ErrTarget
		}
	} else {
		if g.ESIndexName == nil || g.MilvusCollection == nil {
			return zero, generationquery.ErrTarget
		}
		target.ESIndex = *g.ESIndexName
		target.MilvusCollection = *g.MilvusCollection
	}

	if err := target.Validate(); err != nil {
		return zero, err
	}
	return target, nil
}

type KnowledgeActiveRepository struct {
	db *gorm.DB
}

var _ generationquery.Resolver = (*KnowledgeActiveRepository)(nil)

func NewKnowledgeActiveRepository(db *gorm.DB) (*KnowledgeActiveRepository, error) {
	if db == nil {
		return nil, generationquery.ErrResolve
	}
	return &KnowledgeActiveRepository{db: db}, nil
}

func (r *KnowledgeActiveRepository) ResolveActive(ctx context.Context) (generationquery.Target, error) {
	var zero generationquery.Target
	if r == nil || r.db == nil {
		return zero, generationquery.ErrResolve
	}

	bounded, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	var row struct {
		model.KnowledgeIndexGeneration `gorm:"embedded"`
		SelectedEpoch                  uint64
		DesiredEpoch                   uint64
	}

	err := r.db.Session(&gorm.Session{
		Logger: logger.Default.LogMode(logger.Silent),
	}).WithContext(bounded).
		Table("knowledge_index_controls AS c").
		Select("g.*, c.active_epoch AS selected_epoch, c.desired_epoch AS desired_epoch").
		Joins("LEFT JOIN knowledge_index_generations AS g ON g.id = c.active_generation_id").
		Where("c.id = ?", model.KnowledgeIndexControlID).
		Take(&row).Error

	if bounded.Err() != nil {
		return zero, bounded.Err()
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return zero, retrieval.ErrIndexNotReady
	}
	if err != nil {
		return zero, generationquery.ErrResolve
	}
	if row.SelectedEpoch == 0 {
		if row.ID != "" || row.DesiredEpoch == 0 {
			return zero, generationquery.ErrTarget
		}
		return zero, retrieval.ErrIndexNotReady
	}
	if row.SelectedEpoch > row.DesiredEpoch ||
		row.SourceEpoch != row.SelectedEpoch ||
		row.Status != model.KnowledgeGenerationPublished ||
		row.PublishedAt == nil || row.PublishedAt.IsZero() {
		return zero, generationquery.ErrTarget
	}

	return generationTarget(row.KnowledgeIndexGeneration)
}
