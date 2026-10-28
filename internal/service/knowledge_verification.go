package service

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"
	"video-support-agent/internal/model"
)

type KnowledgeDraftVerifier interface {
	VerifyDraft(ctx context.Context, verifierID uint64, input VerifyKnowledgeDraftInput) (*model.RuleVerification, error)
}

func (s *KnowledgeService) VerifyDraft(ctx context.Context, verifierID uint64, input VerifyKnowledgeDraftInput) (*model.RuleVerification, error) {
	if verifierID == 0 {
		return nil, fmt.Errorf("%w: verifier identity is required", ErrUnauthorized)
	}

	if input.VersionID == 0 {
		return nil, fmt.Errorf("%w: version_id must not be zero", ErrInvalidArgument)
	}

	if input.ExpectedContentRevision == 0 {
		return nil, fmt.Errorf("%w: expected_content_revision must not be zero", ErrInvalidArgument)
	}

	input.VerificationNote = strings.TrimSpace(input.VerificationNote)
	if input.VerificationNote == "" {
		return nil, fmt.Errorf("%w: verification_note must not be blank", ErrInvalidArgument)
	}

	if utf8.RuneCountInString(input.VerificationNote) > 2000 {
		return nil, fmt.Errorf("%w: verification_note must not exceed 2000 characters", ErrInvalidArgument)
	}

	if input.EvidenceURL != nil {
		value := strings.TrimSpace(*input.EvidenceURL)
		if value == "" {
			input.EvidenceURL = nil
		} else {
			if err := ValidateKnowledgeOfficialSourceURL(value); err != nil {
				return nil, fmt.Errorf("verify knowledge draft evidence URL: %w", err)
			}
			input.EvidenceURL = &value
		}
	}

	verifier, ok := s.store.(KnowledgeDraftVerifier)
	if !ok {
		return nil, fmt.Errorf("verify knowledge draft: verifier is not configured")
	}

	verification, err := verifier.VerifyDraft(ctx, verifierID, input)
	if err != nil {
		return nil, fmt.Errorf("verify knowledge draft: %w", err)
	}

	return verification, nil
}
