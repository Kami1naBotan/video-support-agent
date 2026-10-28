package service

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"video-support-agent/internal/model"
)

type KnowledgeRevisionCreator interface {
	CreateRevision(ctx context.Context, version *model.RuleVersion, articles []model.RuleArticle) error
}

func (s *KnowledgeService) CreateRevision(ctx context.Context, input CreateKnowledgeRevisionInput) (*model.RuleVersion, error) {
	if input.DocumentID == 0 {
		return nil, fmt.Errorf("%w: document_id must not be zero", ErrInvalidArgument)
	}

	versionName := strings.TrimSpace(input.Version)
	if versionName == "" || utf8.RuneCountInString(versionName) > 64 {
		return nil, fmt.Errorf("%w: version must contain 1 to 64 characters", ErrInvalidArgument)
	}

	switch input.RevisionType {
	case model.RuleRevisionTypeOfficial, model.RuleRevisionTypeEditorial:
	default:
		return nil, fmt.Errorf("%w: revision_type must be official or editorial", ErrInvalidArgument)
	}

	revisionNote := strings.TrimSpace(input.RevisionNote)
	if revisionNote == "" || utf8.RuneCountInString(revisionNote) > 2000 {
		return nil, fmt.Errorf("%w: revision_note must contain 1 to 2000 characters", ErrInvalidArgument)
	}

	var revisionSourceURL *string
	if input.RevisionSourceURL != nil {
		value := strings.TrimSpace(*input.RevisionSourceURL)
		if value != "" {
			if err := ValidateKnowledgeOfficialSourceURL(value); err != nil {
				return nil, fmt.Errorf("validate knowledge revision source URL: %w", err)
			}

			revisionSourceURL = &value
		}
	}

	if input.RevisionType == model.RuleRevisionTypeOfficial &&
		revisionSourceURL == nil {
		return nil, fmt.Errorf("%w: official revision requires revision_source_url", ErrInvalidArgument)
	}

	version, articles, err := normalizeKnowledgeContent(
		model.RuleVersion{
			CategoryID:  input.CategoryID,
			Title:       input.Title,
			Content:     input.Content,
			SourceURL:   input.SourceURL,
			EffectiveAt: input.EffectiveAt,
			ExpiresAt:   input.ExpiresAt,
		},
		input.Articles,
	)
	if err != nil {
		return nil, fmt.Errorf("create knowledge revision: %w", err)
	}

	version.DocumentID = input.DocumentID
	version.Version = versionName
	version.RevisionType = input.RevisionType
	version.RevisionNote = revisionNote
	version.RevisionSourceURL = revisionSourceURL
	version.ContentRevision = 1
	version.Status = model.RuleVersionStatusDraft

	creator, ok := s.store.(KnowledgeRevisionCreator)
	if !ok {
		return nil, fmt.Errorf("create knowledge revision: revision creator is not configured")
	}

	if err := creator.CreateRevision(ctx, &version, articles); err != nil {
		return nil, fmt.Errorf("create knowledge revision: %w", err)
	}

	return &version, nil
}
