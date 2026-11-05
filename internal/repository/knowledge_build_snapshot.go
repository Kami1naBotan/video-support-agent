package repository

import (
	"context"
	"errors"
	"time"

	indexing "video-support-agent/internal/index"
	"video-support-agent/internal/knowledgeworker"
	"video-support-agent/internal/model"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func snapshotError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err == nil {
		return nil
	}
	for _, known := range []error{
		knowledgeworker.ErrBuildConfig,
		knowledgeworker.ErrBuildState,
		knowledgeworker.ErrBuildLeaseLost,
		knowledgeworker.ErrBuildObsolete,
		knowledgeworker.ErrProjectionConflict,
		knowledgeworker.ErrProjectionLimit,
	} {
		if errors.Is(err, known) {
			return known
		}
	}
	if errors.Is(err, indexing.ErrExportLimitExceeded) {
		return knowledgeworker.ErrProjectionLimit
	}
	if errors.Is(err, indexing.ErrInvalidSource) || errors.Is(err, gorm.ErrRecordNotFound) {
		return knowledgeworker.ErrProjectionConflict
	}
	return knowledgeworker.ErrBuildStore
}

func (r *KnowledgeBuildRepository) PrepareSnapshot(ctx context.Context, claim knowledgeworker.BuildClaim) (*knowledgeworker.BuildSnapshot, error) {
	bounded, cancel := context.WithTimeout(ctx, knowledgeworker.BuildTimeout)
	defer cancel()

	if err := r.Renew(bounded, claim); err != nil {
		return nil, err
	}

	db := r.db.Session(&gorm.Session{Logger: logger.Default.LogMode(logger.Silent)}).WithContext(bounded)

	documents, err := projectionDocuments(db)
	if err != nil {
		return nil, snapshotError(bounded, err)
	}

	budget := &projectionBudget{}
	for _, document := range documents {
		if document.CurrentVersionID == nil {
			continue
		}
		if err := r.Renew(bounded, claim); err != nil {
			return nil, err
		}
		if err := r.prepareDocumentProjection(bounded, document.ID, budget); err != nil {
			return nil, snapshotError(bounded, err)
		}
	}

	return r.readBuildSnapshot(bounded, claim)
}

func (r *KnowledgeBuildRepository) readBuildSnapshot(ctx context.Context, claim knowledgeworker.BuildClaim) (*knowledgeworker.BuildSnapshot, error) {
	bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	var capturedAt time.Time
	revisions := make([]knowledgeworker.DocumentRevision, 0)

	db := r.db.Session(&gorm.Session{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	source := NewKnowledgeRepositoryWithClock(db, func() time.Time {
		return capturedAt
	})

	snapshot, err := source.exportCurrent(bounded, func(tx *gorm.DB) error {
		var control model.KnowledgeIndexControl
		if err := tx.Where("id = ?", model.KnowledgeIndexControlID).
			Take(&control).Error; err != nil {
			return err
		}
		if control.DesiredEpoch != claim.SourceEpoch {
			return knowledgeworker.ErrBuildObsolete
		}
		if control.OwnerToken == nil || *control.OwnerToken != claim.OwnerToken || control.LeaseUntil == nil {
			return knowledgeworker.ErrBuildLeaseLost
		}

		now, err := buildDatabaseTime(tx)
		if err != nil {
			return err
		}
		if !control.LeaseUntil.After(now) {
			return knowledgeworker.ErrBuildLeaseLost
		}
		capturedAt = now

		documents, err := projectionDocuments(tx)
		if err != nil {
			return err
		}
		budget := &projectionBudget{}

		for _, document := range documents {
			if err := bounded.Err(); err != nil {
				return err
			}

			revisions = append(revisions, knowledgeworker.DocumentRevision{
				DocumentID:          document.ID,
				PublicationRevision: document.PublicationRevision,
				CurrentVersionID:    document.CurrentVersionID,
			})

			if document.CurrentVersionID == nil {
				continue
			}
			version, err := projectionVersion(tx, document, false)
			if err != nil {
				return err
			}
			if err := checkArticleProjections(tx, version.ID, false, budget); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, snapshotError(bounded, err)
	}

	prepared, err := indexing.PrepareSnapshot(bounded, snapshot)
	if err != nil {
		return nil, snapshotError(bounded, err)
	}

	if err := r.Renew(bounded, claim); err != nil {
		return nil, err
	}

	result, err := knowledgeworker.NewBuildSnapshot(bounded, claim.SourceEpoch, prepared, revisions)
	if err != nil {
		return nil, snapshotError(bounded, err)
	}
	return result, nil
}
