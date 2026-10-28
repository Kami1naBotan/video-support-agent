package service

import (
	"context"
	"fmt"

	"video-support-agent/internal/model"
)

type KnowledgeDocumentStateReader interface {
	GetDocumentState(ctx context.Context, documentID uint64) (*model.KnowledgeDocument, error)
}

type KnowledgeDocumentState struct {
	DocumentID          uint64
	CurrentVersionID    *uint64
	PublicationRevision uint64
}

func (s *KnowledgeService) GetDocumentState(ctx context.Context, documentID uint64) (*KnowledgeDocumentState, error) {
	if documentID == 0 {
		return nil, fmt.Errorf("%w: document_id must not be zero", ErrInvalidArgument)
	}

	reader, ok := s.store.(KnowledgeDocumentStateReader)
	if !ok {
		return nil, fmt.Errorf("get knowledge document state: reader is not configured")
	}

	document, err := reader.GetDocumentState(ctx, documentID)
	if err != nil {
		return nil, fmt.Errorf("get knowledge document state: %w", err)
	}

	if document == nil {
		return nil, fmt.Errorf("get knowledge document state: reader returned an empty document")
	}

	var currentVersionID *uint64
	if document.CurrentVersionID != nil {
		value := *document.CurrentVersionID
		currentVersionID = &value
	}

	return &KnowledgeDocumentState{
		DocumentID:          document.ID,
		CurrentVersionID:    currentVersionID,
		PublicationRevision: document.PublicationRevision,
	}, nil
}
