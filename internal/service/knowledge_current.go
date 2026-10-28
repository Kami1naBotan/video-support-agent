package service

import (
	"context"
	"fmt"

	"video-support-agent/internal/model"
)

type KnowledgeCurrentReader interface {
	GetCurrentByDocumentID(ctx context.Context, documentID uint64) (*model.RuleVersion, []model.RuleArticle, error)
}

type KnowledgeCurrentDetail struct {
	Version  *model.RuleVersion
	Articles []model.RuleArticle
}

func (s *KnowledgeService) GetCurrentByDocumentID(ctx context.Context, documentID uint64) (*KnowledgeCurrentDetail, error) {
	if documentID == 0 {
		return nil, fmt.Errorf("%w: document_id must not be zero", ErrInvalidArgument)
	}

	reader, ok := s.store.(KnowledgeCurrentReader)
	if !ok {
		return nil, fmt.Errorf("get current knowledge: reader is not configured")
	}

	version, articles, err := reader.GetCurrentByDocumentID(ctx, documentID)
	if err != nil {
		return nil, fmt.Errorf("get current knowledge: %w", err)
	}

	if version == nil {
		return nil, fmt.Errorf("get current knowledge: reader returned an empty version")
	}

	return &KnowledgeCurrentDetail{
		Version:  version,
		Articles: articles,
	}, nil
}
