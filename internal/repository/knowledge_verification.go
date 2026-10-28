package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
	"video-support-agent/internal/model"
	"video-support-agent/internal/service"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var _ service.KnowledgeDraftVerifier = (*KnowledgeRepository)(nil)

const knowledgeVerificationSnapshotSchemaVersion uint32 = 2

type knowledgeVerificationSnapshot struct {
	VersionID         uint64                         `json:"version_id"`
	Version           string                         `json:"version"`
	ContentRevision   uint64                         `json:"content_revision"`
	CategoryID        uint64                         `json:"category_id"`
	RevisionType      model.RuleRevisionType         `json:"revision_type"`
	RevisionNote      string                         `json:"revision_note"`
	RevisionSourceURL *string                        `json:"revision_source_url"`
	Title             string                         `json:"title"`
	Content           string                         `json:"content"`
	SourceURL         string                         `json:"source_url"`
	EffectiveAt       *time.Time                     `json:"effective_at"`
	ExpiresAt         *time.Time                     `json:"expires_at"`
	Articles          []knowledgeVerificationArticle `json:"articles"`
}

type knowledgeVerificationArticle struct {
	ArticleOrder  uint32 `json:"article_order"`
	SectionPath   string `json:"section_path"`
	ArticleNumber string `json:"article_number"`
	Content       string `json:"content"`
}

func (r *KnowledgeRepository) VerifyDraft(ctx context.Context, verifierID uint64, input service.VerifyKnowledgeDraftInput) (*model.RuleVerification, error) {
	if verifierID == 0 {
		return nil, fmt.Errorf("%w: verifier identity is required", service.ErrUnauthorized)
	}

	if input.VersionID == 0 || input.ExpectedContentRevision == 0 {
		return nil, fmt.Errorf("%w: version_id and expected_content_revision must not be zero", service.ErrInvalidArgument)
	}

	note := strings.TrimSpace(input.VerificationNote)
	if note == "" || utf8.RuneCountInString(note) > 2000 {
		return nil, fmt.Errorf("%w: verification_note must contain 1 to 2000 characters", service.ErrInvalidArgument)
	}

	var evidenceURL *string
	if input.EvidenceURL != nil {
		value := strings.TrimSpace(*input.EvidenceURL)
		if value != "" {
			if err := service.ValidateKnowledgeOfficialSourceURL(value); err != nil {
				return nil, fmt.Errorf("validate verification evidence URL: %w", err)
			}
			evidenceURL = &value
		}
	}

	var saved model.RuleVerification

	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var version model.RuleVersion

		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", input.VersionID).First(&version).Error

		if errors.Is(err, gorm.ErrRecordNotFound) {
			return service.ErrKnowledgeVersionNotFound
		}
		if err != nil {
			return fmt.Errorf("lock knowledge version for verification: %w", err)
		}

		if version.Status != model.RuleVersionStatusDraft {
			return service.ErrKnowledgeVersionNotEditable
		}

		if version.ContentRevision != input.ExpectedContentRevision {
			return service.ErrKnowledgeContentConflict
		}

		if err := service.ValidateKnowledgeOfficialSourceURL(version.SourceURL); err != nil {
			return fmt.Errorf("validate knowledge version source URL: %w", err)
		}

		var articles []model.RuleArticle
		err = tx.Clauses(clause.Locking{Strength: "SHARE"}).Where("version_id = ?", version.ID).Order("article_order ASC").Find(&articles).Error
		if err != nil {
			return fmt.Errorf("read knowledge articles for verification: %w", err)
		}

		if len(articles) == 0 {
			return fmt.Errorf("verify knowledge draft: stored versin has no articles")
		}

		snapshotArticles := make([]knowledgeVerificationArticle, len(articles))
		for i, article := range articles {
			snapshotArticles[i] = knowledgeVerificationArticle{
				ArticleOrder:  article.ArticleOrder,
				SectionPath:   article.SectionPath,
				ArticleNumber: article.ArticleNumber,
				Content:       article.Content,
			}
		}

		snapshot := knowledgeVerificationSnapshot{
			VersionID:         version.ID,
			Version:           version.Version,
			ContentRevision:   version.ContentRevision,
			CategoryID:        version.CategoryID,
			RevisionType:      version.RevisionType,
			RevisionNote:      version.RevisionNote,
			RevisionSourceURL: version.RevisionSourceURL,
			Title:             version.Title,
			Content:           version.Content,
			SourceURL:         version.SourceURL,
			EffectiveAt:       version.EffectiveAt,
			ExpiresAt:         version.ExpiresAt,
			Articles:          snapshotArticles,
		}

		snapshotJSON, err := json.Marshal(snapshot)
		if err != nil {
			return fmt.Errorf("encode knowledge verification snapshot: %w", err)
		}

		saved = model.RuleVerification{
			VersionID:               version.ID,
			VerifiedContentRevision: version.ContentRevision,
			VerifiedBy:              verifierID,
			VerifiedAt:              time.Now().UTC().Truncate(time.Millisecond),
			VerificationNote:        note,
			EvidenceURL:             evidenceURL,
			SnapshotSchemaVersion:   knowledgeVerificationSnapshotSchemaVersion,
			ContentSnapshot:         string(snapshotJSON),
		}

		if err := tx.Omit(clause.Associations).Create(&saved).Error; err != nil {
			return fmt.Errorf("insert knowledge verification: %w", err)
		}

		return nil
	})

	if err != nil {
		return nil, fmt.Errorf("verify knowledge draft transaction: %w", err)
	}

	return &saved, nil
}
