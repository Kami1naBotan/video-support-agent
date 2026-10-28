package service

import (
	"context"
	"fmt"

	"video-support-agent/internal/model"
)

type KnowledgeHistoryState struct {
	IsCurrent          bool
	IsSuperseded       bool
	IsExpired          bool
	IsWithdrawn        bool
	IsCurrentEffective bool
}

type KnowledgeHistoryDetail struct {
	Version  *model.RuleVersion
	Articles []model.RuleArticle
	State    KnowledgeHistoryState
}

type KnowledgeHistoryArticleDetail struct {
	Version *model.RuleVersion
	Article *model.RuleArticle
	State   KnowledgeHistoryState
}

type KnowledgeHistoryReader interface {
	GetHistoryByVersionID(ctx context.Context, versionID uint64) (*KnowledgeHistoryDetail, error)
	GetHistoryArticle(ctx context.Context, versionID uint64, articleID uint64) (*KnowledgeHistoryArticleDetail, error)
}

func (s *KnowledgeService) GetHistoryByVersionID(ctx context.Context, versionID uint64) (*KnowledgeHistoryDetail, error) {
	if versionID == 0 {
		return nil, fmt.Errorf("%w: version_id must not be zero", ErrInvalidArgument)
	}

	reader, ok := s.store.(KnowledgeHistoryReader)
	if !ok {
		return nil, fmt.Errorf("get knowledge history: reader is not configured")
	}

	detail, err := reader.GetHistoryByVersionID(ctx, versionID)
	if err != nil {
		return nil, fmt.Errorf("get knowledge history: %w", err)
	}
	if detail == nil || detail.Version == nil {
		return nil, fmt.Errorf("get knowledge history: reader returned an empty result")
	}

	if err := validateKnowledgeHistoryVersion(detail.Version, versionID); err != nil {
		return nil, err
	}

	for _, article := range detail.Articles {
		if article.VersionID != versionID {
			return nil, fmt.Errorf("get knowledge history: reader returned an unrelated article")
		}
	}

	return detail, nil
}

func (s *KnowledgeService) GetHistoryArticle(ctx context.Context, versionID uint64, articleID uint64) (*KnowledgeHistoryArticleDetail, error) {
	if versionID == 0 || articleID == 0 {
		return nil, fmt.Errorf("%w: version_id and article_id must not be zero", ErrInvalidArgument)
	}

	reader, ok := s.store.(KnowledgeHistoryReader)
	if !ok {
		return nil, fmt.Errorf("get knowledge history article: reader is not configured")
	}

	detail, err := reader.GetHistoryArticle(ctx, versionID, articleID)
	if err != nil {
		return nil, fmt.Errorf("get knowledge history article: %w", err)
	}
	if detail == nil || detail.Version == nil || detail.Article == nil {
		return nil, fmt.Errorf("get knowledge history article: reader returned an empty result")
	}

	if err := validateKnowledgeHistoryVersion(detail.Version, versionID); err != nil {
		return nil, err
	}

	if detail.Article.ID != articleID || detail.Article.VersionID != versionID {
		return nil, ErrKnowledgeArticleNotFound
	}

	return detail, nil
}

func validateKnowledgeHistoryVersion(version *model.RuleVersion, expectedVersionID uint64) error {
	if version.ID != expectedVersionID {
		return ErrKnowledgeVersionNotFound
	}

	if version.PublishedAt == nil {
		return ErrKnowledgeVersionNotFound
	}

	if version.Status != model.RuleVersionStatusPublished && version.Status != model.RuleVersionStatusWithdrawn {
		return ErrKnowledgeVersionNotFound
	}

	return nil
}
