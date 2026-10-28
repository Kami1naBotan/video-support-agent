package service

import (
	"context"
	"fmt"
	"time"
)

type KnowledgePublicationResult struct {
	DocumentID          uint64
	VersionID           uint64
	PublicationRevision uint64
	PublishedAt         time.Time
}

type KnowledgeVersionPublisher interface {
	PublishVersion(ctx context.Context, operatorID uint64, input PublishKnowledgeVersionInput) (*KnowledgePublicationResult, error)
}

func (s *KnowledgeService) PublishVersion(ctx context.Context, operatorID uint64, input PublishKnowledgeVersionInput) (*KnowledgePublicationResult, error) {
	if operatorID == 0 {
		return nil, fmt.Errorf("%w: operator identity is required", ErrUnauthorized)
	}

	if input.DocumentID == 0 {
		return nil, fmt.Errorf("%w: document_id must not be zero", ErrInvalidArgument)
	}

	if input.VersionID == 0 {
		return nil, fmt.Errorf("%w: version_id must not be zero", ErrInvalidArgument)
	}

	publisher, ok := s.store.(KnowledgeVersionPublisher)
	if !ok {
		return nil, fmt.Errorf("publish knowledge version: publisher is not configured")
	}

	result, err := publisher.PublishVersion(ctx, operatorID, input)
	if err != nil {
		return nil, fmt.Errorf("publish knowledge version: %w", err)
	}

	if result == nil {
		return nil, fmt.Errorf("publish knowledge version: publisher returned an empty result")
	}

	return result, nil
}
