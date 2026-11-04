package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	"video-support-agent/internal/model"
	"video-support-agent/internal/service"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type appealTxEvidence struct {
	db        *gorm.DB
	called    bool
	refs      []model.AppealRuleEvidence
	at        time.Time
	available bool
	err       error
}

func orderedAppealEvidence(refs []model.AppealRuleEvidence) ([]model.AppealRuleEvidence, error) {
	if len(refs) > 8 {
		return nil, service.ErrAppealInvalidPolicy
	}
	result := append([]model.AppealRuleEvidence{}, refs...)
	articles := make(map[uint64]bool)
	documents := make(map[uint64][2]uint64)
	for _, ref := range result {
		identity := [2]uint64{ref.VersionID, ref.PublicationRevision}
		previous, exists := documents[ref.DocumentID]
		if ref.DocumentID == 0 || ref.VersionID == 0 || ref.ArticleID == 0 || ref.PublicationRevision == 0 || articles[ref.ArticleID] || (exists && previous != identity) {
			return nil, service.ErrAppealInvalidPolicy
		}
		articles[ref.ArticleID] = true
		documents[ref.DocumentID] = identity
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].DocumentID != result[j].DocumentID {
			return result[i].DocumentID < result[j].DocumentID
		}
		return result[i].ArticleID < result[j].ArticleID
	})
	return result, nil
}

func (c *appealTxEvidence) CheckAppealEvidence(ctx context.Context, refs []model.AppealRuleEvidence, at time.Time) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	ordered, err := orderedAppealEvidence(refs)
	if err != nil {
		return false, err
	}
	at = at.UTC().Truncate(time.Millisecond)
	if at.IsZero() || at.Year() < 1000 || at.Year() > 9999 {
		return false, service.ErrInvalidArgument
	}
	if c.called {
		if !slices.Equal(c.refs, ordered) || !c.at.Equal(at) {
			return false, service.ErrAppealInvalidPolicy
		}
		return c.available, c.err
	}
	c.called, c.refs, c.at = true, ordered, at
	c.available, c.err = readAppealEvidence(c.db.WithContext(ctx), ordered, at)
	return c.available, c.err
}

func readAppealEvidence(db *gorm.DB, refs []model.AppealRuleEvidence, at time.Time) (bool, error) {
	if len(refs) == 0 {
		return false, nil
	}
	locked := db.Clauses(clause.Locking{Strength: "SHARE"})
	var previousDocument uint64
	for _, ref := range refs {
		if ref.DocumentID != previousDocument {
			var document model.KnowledgeDocument
			err := locked.Session(&gorm.Session{}).Select("id", "current_version_id", "publication_revision").
				Where("id = ?", ref.DocumentID).Take(&document).Error
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return false, nil
			}
			if err != nil {
				return false, appealStorageError(err)
			}
			if document.CurrentVersionID == nil || *document.CurrentVersionID != ref.VersionID || document.PublicationRevision != ref.PublicationRevision {
				return false, nil
			}
			var version model.RuleVersion
			err = locked.Session(&gorm.Session{}).Select("id", "document_id", "content_revision", "status", "published_at", "withdrawn_at", "effective_at", "expires_at").
				Where("id = ? AND document_id = ?", ref.VersionID, ref.DocumentID).Take(&version).Error
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return false, nil
			}
			if err != nil {
				return false, appealStorageError(err)
			}
			if version.Status != model.RuleVersionStatusPublished || version.WithdrawnAt != nil ||
				version.PublishedAt == nil || version.PublishedAt.After(at) ||
				version.EffectiveAt == nil || version.EffectiveAt.After(at) ||
				(version.ExpiresAt != nil && !at.Before(*version.ExpiresAt)) {
				return false, nil
			}
			var verification model.RuleVerification
			err = locked.Session(&gorm.Session{}).Select("id", "verified_content_revision", "verified_at").
				Where("version_id = ?", ref.VersionID).Order("id DESC").Take(&verification).Error
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return false, nil
			}
			if err != nil {
				return false, appealStorageError(err)
			}
			if version.ContentRevision == 0 || verification.VerifiedContentRevision != version.ContentRevision || verification.VerifiedAt.IsZero() || verification.VerifiedAt.After(at) {
				return false, nil
			}
			previousDocument = ref.DocumentID
		}
		var article model.RuleArticle
		err := locked.Session(&gorm.Session{}).Select("id").
			Where("id = ? AND version_id = ?", ref.ArticleID, ref.VersionID).Take(&article).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, nil
		}
		if err != nil {
			return false, appealStorageError(err)
		}
	}
	return true, nil
}

func (r *AppealTicketRepository) CheckAppealEvidence(ctx context.Context, refs []model.AppealRuleEvidence, at time.Time) (bool, error) {
	ctx, release := service.WithAppealFinalization(ctx)
	defer release()
	workCtx, cancelWork := context.WithCancel(ctx)
	defer cancelWork()
	tx := r.db.WithContext(workCtx).Begin(&sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if tx.Error != nil {
		return false, appealStorageError(tx.Error)
	}
	sqlTx := tx.Statement.ConnPool.(*sql.Tx)
	checker := &appealTxEvidence{db: tx}
	available, checkErr := checker.CheckAppealEvidence(workCtx, refs, at)
	cancelWork()
	done := make(chan error, 1)
	go func() { done <- sqlTx.Rollback() }()
	_, cleanupErr := awaitAppealEnd(ctx, done)
	if errors.Is(cleanupErr, sql.ErrTxDone) {
		cleanupErr = nil
	}
	if cleanupErr != nil {
		return false, fmt.Errorf("%w: %w", service.ErrAppealStorage, errors.Join(checkErr, cleanupErr))
	}
	if checkErr != nil {
		return false, checkErr
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return available, nil
}
