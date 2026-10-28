package service

import (
	"context"
	"fmt"
	"time"
)

type KnowledgeWithdrawalResult struct {
	DocumentID          uint64
	VersionID           uint64
	PublicationRevision uint64
	WithdrawnAt         time.Time
}

type KnowledgeVersionWithdrawer interface {
	WithdrawVersion(ctx context.Context, operatorID uint64, input WithdrawKnowledgeVersionInput) (*KnowledgeWithdrawalResult, error)
}

func (s *KnowledgeService) WithdrawVersion(ctx context.Context, operatorID uint64, input WithdrawKnowledgeVersionInput) (*KnowledgeWithdrawalResult, error) {
	if operatorID == 0 {
		return nil, fmt.Errorf("%w: operator identity is required", ErrUnauthorized)
	}

	if input.DocumentID == 0 {
		return nil, fmt.Errorf("%w: document_id must not be zero", ErrInvalidArgument)
	}

	if input.VersionID == 0 {
		return nil, fmt.Errorf("%w: version_id must not be zero", ErrInvalidArgument)
	}

	withdrawer, ok := s.store.(KnowledgeVersionWithdrawer)
	if !ok {
		return nil, fmt.Errorf("withdraw knowledge version: withdrawer is not configured")
	}

	result, err := withdrawer.WithdrawVersion(ctx, operatorID, input)
	if err != nil {
		return nil, fmt.Errorf("withdraw knowledge version: %w", err)
	}

	if result == nil {
		return nil, fmt.Errorf("withdraw knowledge version: withdrawer returned an empty result")
	}

	return result, nil
}
