package service

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"
	"video-support-agent/internal/model"
)

type KnowledgeStore interface {
	CreateDraft(ctx context.Context, document *model.KnowledgeDocument, version *model.RuleVersion, articles []model.RuleArticle) error
	GetDraftByID(ctx context.Context, versionID uint64) (*model.RuleVersion, []model.RuleArticle, error)
}

type KnowledgeDraftEditor interface {
	EditDraft(ctx context.Context, versionID uint64, expectedContentRevision uint64, content model.RuleVersion, articles []model.RuleArticle) (*model.RuleVersion, error)
}

type KnowledgeDraftVerificationEditor interface {
	EditDraftWithVerification(ctx context.Context, versionID uint64, expectedRevision uint64, content model.RuleVersion, articles []model.RuleArticle) (*model.RuleVersion, *model.RuleVerification, error)
}

type KnowledgeDraftDetailReader interface {
	GetDraftDetailByID(ctx context.Context, versionID uint64) (*model.RuleVersion, []model.RuleArticle, *model.RuleVerification, error)
}

type KnowledgeService struct {
	store KnowledgeStore
}

type KnowledgeDraftDetail struct {
	Version      *model.RuleVersion
	Articles     []model.RuleArticle
	Verification *model.RuleVerification
}

type KnowledgeDraftEditResult struct {
	Version      *model.RuleVersion
	Verification *model.RuleVerification
}

func NewKnowledgeService(store KnowledgeStore) *KnowledgeService {
	return &KnowledgeService{store: store}
}

func (s *KnowledgeService) CreateDraft(ctx context.Context, input CreateKnowledgeDraftInput) (*model.RuleVersion, error) {
	input.DocumentKey = strings.TrimSpace(input.DocumentKey)

	if input.DocumentKey == "" {
		return nil, fmt.Errorf("%w: document_key must not be blank", ErrInvalidArgument)
	}

	if utf8.RuneCountInString(input.DocumentKey) > 100 {
		return nil, fmt.Errorf("%w: document_key must not exceed 100 characters", ErrInvalidArgument)
	}

	input.Version = strings.TrimSpace(input.Version)

	if input.Version == "" {
		return nil, fmt.Errorf("%w: version must not be blank", ErrInvalidArgument)
	}

	if utf8.RuneCountInString(input.Version) > 64 {
		return nil, fmt.Errorf("%w: version must not exceed 64 characters", ErrInvalidArgument)
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
		return nil, fmt.Errorf("create knowledge draft: %w", err)
	}

	document := model.KnowledgeDocument{
		DocumentKey:         input.DocumentKey,
		CurrentVersionID:    nil,
		PublicationRevision: 0,
	}

	version.Version = input.Version
	version.ContentRevision = 1
	version.Status = model.RuleVersionStatusDraft

	if s.store == nil {
		return nil, fmt.Errorf("create knowledge draft: store is not configured")
	}

	if err := s.store.CreateDraft(ctx, &document, &version, articles); err != nil {
		return nil, fmt.Errorf("create knowledge draft: %w", err)
	}

	return &version, nil
}

func (s *KnowledgeService) EditDraftWithVerification(ctx context.Context, input EditKnowledgeDraftInput) (*KnowledgeDraftEditResult, error) {
	if input.VersionID == 0 {
		return nil, fmt.Errorf("%w: version_id must not be zero", ErrInvalidArgument)
	}

	if input.ExpectedContentRevision == 0 {
		return nil, fmt.Errorf("%w: expected_content_revision must not be zero", ErrInvalidArgument)
	}

	content, articles, err := normalizeKnowledgeContent(
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
		return nil, fmt.Errorf("edit knowledge draft: %w", err)
	}

	editor, ok := s.store.(KnowledgeDraftVerificationEditor)
	if !ok {
		return nil, fmt.Errorf("edit knowledge draft: verification editor is not configured")
	}

	version, verification, err := editor.EditDraftWithVerification(ctx, input.VersionID, input.ExpectedContentRevision, content, articles)
	if err != nil {
		return nil, fmt.Errorf("edit knowledge draft: %w", err)
	}

	return &KnowledgeDraftEditResult{
		Version:      version,
		Verification: verification,
	}, nil
}

func (s *KnowledgeService) GetDraftByID(ctx context.Context, versionID uint64) (*KnowledgeDraftDetail, error) {
	if versionID == 0 {
		return nil, fmt.Errorf("%w: version_id must not be zero", ErrInvalidArgument)
	}

	if s.store == nil {
		return nil, fmt.Errorf("get knowledge draft: store is not configured")
	}

	version, articles, err := s.store.GetDraftByID(ctx, versionID)
	if err != nil {
		return nil, fmt.Errorf("get knowledge draft: %w", err)
	}

	return &KnowledgeDraftDetail{
		Version:  version,
		Articles: articles,
	}, nil
}

func (s *KnowledgeService) GetDraftDetailByID(ctx context.Context, versionID uint64) (*KnowledgeDraftDetail, error) {
	if versionID == 0 {
		return nil, fmt.Errorf("%w: version_id must not be zero", ErrInvalidArgument)
	}

	reader, ok := s.store.(KnowledgeDraftDetailReader)
	if !ok {
		return nil, fmt.Errorf("get knowledge draft: detail reader is not configured")
	}

	version, articles, verification, err := reader.GetDraftDetailByID(ctx, versionID)
	if err != nil {
		return nil, fmt.Errorf("get knowledge draft detail: %w", err)
	}

	return &KnowledgeDraftDetail{
		Version:      version,
		Articles:     articles,
		Verification: verification,
	}, nil
}

func normalizeKnowledgeContent(version model.RuleVersion, inputArticles []CreateRuleArticleInput) (model.RuleVersion, []model.RuleArticle, error) {
	if version.CategoryID == 0 {
		return model.RuleVersion{}, nil, fmt.Errorf("%w: category_id must not be zero", ErrInvalidArgument)
	}

	version.Title = strings.TrimSpace(version.Title)

	if version.Title == "" {
		return model.RuleVersion{}, nil, fmt.Errorf("%w: title must not be blank", ErrInvalidArgument)
	}

	if utf8.RuneCountInString(version.Title) > 255 {
		return model.RuleVersion{}, nil, fmt.Errorf("%w: title must not exceed 255 characters", ErrInvalidArgument)
	}

	if strings.TrimSpace(version.Content) == "" {
		return model.RuleVersion{}, nil, fmt.Errorf("%w: content must not be blank", ErrInvalidArgument)
	}

	version.SourceURL = strings.TrimSpace(version.SourceURL)

	if version.SourceURL == "" {
		return model.RuleVersion{}, nil, fmt.Errorf("%w: source_url must not be blank", ErrInvalidArgument)
	}

	if utf8.RuneCountInString(version.SourceURL) > 2048 {
		return model.RuleVersion{}, nil, fmt.Errorf("%w: source_url must not exceed 2048 characters", ErrInvalidArgument)
	}

	sourceURL, err := url.Parse(version.SourceURL)
	if err != nil {
		return model.RuleVersion{}, nil, fmt.Errorf("%w: source_url is malformed", ErrInvalidArgument)
	}

	if sourceURL.Scheme != "http" && sourceURL.Scheme != "https" {
		return model.RuleVersion{}, nil, fmt.Errorf("%w: source_url must use http or https", ErrInvalidArgument)
	}

	if sourceURL.Hostname() == "" {
		return model.RuleVersion{}, nil, fmt.Errorf("%w: source_url must contain a hostname", ErrInvalidArgument)
	}

	if sourceURL.User != nil {
		return model.RuleVersion{}, nil, fmt.Errorf("%w: source_url must not contain credentials", ErrInvalidArgument)
	}

	if version.EffectiveAt != nil && version.ExpiresAt != nil {
		if !version.ExpiresAt.After(*version.EffectiveAt) {
			return model.RuleVersion{}, nil, fmt.Errorf("%w: expires_at must be after effective_at", ErrInvalidArgument)
		}
	}

	if len(inputArticles) == 0 {
		return model.RuleVersion{}, nil, fmt.Errorf("%w: articles must contain at least one article", ErrInvalidArgument)
	}

	if uint64(len(inputArticles)) > uint64(^uint32(0)) {
		return model.RuleVersion{}, nil, fmt.Errorf("%w: too many articles", ErrInvalidArgument)
	}

	articles := make([]model.RuleArticle, len(inputArticles))

	for i, inputArticle := range inputArticles {
		sectionPath := strings.TrimSpace(inputArticle.SectionPath)
		articleNumber := strings.TrimSpace(inputArticle.ArticleNumber)

		if utf8.RuneCountInString(sectionPath) > 500 {
			return model.RuleVersion{}, nil, fmt.Errorf("%w: article[%d].section_path must not exceed 500 characters", ErrInvalidArgument, i)
		}

		if utf8.RuneCountInString(articleNumber) > 64 {
			return model.RuleVersion{}, nil, fmt.Errorf("%w: article[%d].article_number must not exceed 64 characters", ErrInvalidArgument, i)
		}

		if strings.TrimSpace(inputArticle.Content) == "" {
			return model.RuleVersion{}, nil, fmt.Errorf("%w: article[%d].content must not be blank", ErrInvalidArgument, i)
		}

		articles[i] = model.RuleArticle{
			ArticleOrder:  uint32(i + 1),
			SectionPath:   sectionPath,
			ArticleNumber: articleNumber,
			Content:       inputArticle.Content,
		}
	}

	if version.EffectiveAt != nil {
		value := *version.EffectiveAt
		version.EffectiveAt = &value
	}

	if version.ExpiresAt != nil {
		value := *version.ExpiresAt
		version.ExpiresAt = &value
	}

	return version, articles, nil
}

func (s *KnowledgeService) EditDraft(ctx context.Context, input EditKnowledgeDraftInput) (*model.RuleVersion, error) {
	result, err := s.EditDraftWithVerification(ctx, input)
	if err != nil {
		return nil, err
	}

	return result.Version, nil
}
