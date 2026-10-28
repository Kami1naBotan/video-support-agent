package repository

import (
	"context"
	"errors"
	"fmt"

	"video-support-agent/internal/model"
	"video-support-agent/internal/service"

	"gorm.io/gorm"
)

var _ service.KnowledgeDocumentStateReader = (*KnowledgeRepository)(nil)

func (r *KnowledgeRepository) GetDocumentState(ctx context.Context, documentID uint64) (*model.KnowledgeDocument, error) {
	if documentID == 0 {
		return nil, fmt.Errorf("%w: document_id must not be zero", service.ErrInvalidArgument)
	}

	var document model.KnowledgeDocument
	err := r.db.WithContext(ctx).Select("id", "current_version_id", "publication_revision").Where("id = ?", documentID).First(&document).Error

	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, service.ErrKnowledgeDocumentNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("find knowledge document state: %w", err)
	}

	return &document, nil
}
