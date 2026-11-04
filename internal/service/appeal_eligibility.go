package service

import (
	"context"
	"fmt"
	"time"

	"video-support-agent/internal/model"
)

type AppealPolicyDefinition struct {
	Code                   string
	Revision               string
	SubjectType            model.AppealSubjectType
	EvidenceRefs           []model.AppealRuleEvidence
	RequiredVerifiedFields []string
}

type AppealPolicyEvaluation struct {
	Decision       model.AppealEligibilityDecision
	ReasonCode     string
	VerifiedFields []string
}

type AppealPolicy interface {
	Definition() AppealPolicyDefinition
	Evaluate(context.Context, AppealFacts, time.Time) (AppealPolicyEvaluation, error)
}

type AppealPolicyProvider interface {
	Resolve(context.Context, model.AppealSubjectType) (AppealPolicy, error)
}

type NoAppealPolicies struct{}

func (NoAppealPolicies) Resolve(ctx context.Context, _ model.AppealSubjectType) (AppealPolicy, error) {
	return nil, ctx.Err()
}

type AppealEligibilityService struct {
	policies AppealPolicyProvider
}

func NewAppealEligibilityService(policies AppealPolicyProvider) (*AppealEligibilityService, error) {
	if policies == nil {
		return nil, ErrAppealConfiguration
	}
	return &AppealEligibilityService{policies: policies}, nil
}

func (s *AppealEligibilityService) Evaluate(ctx context.Context, raw AppealFacts, at time.Time, evidence AppealEvidenceChecker) (result model.AppealEligibilitySnapshot, returnErr error) {
	defer func() {
		if err := ctx.Err(); err != nil {
			result = model.AppealEligibilitySnapshot{}
			returnErr = err
		}
	}()
	if err := ctx.Err(); err != nil {
		return model.AppealEligibilitySnapshot{}, err
	}
	if s == nil || s.policies == nil {
		return model.AppealEligibilitySnapshot{}, ErrAppealConfiguration
	}
	at = at.UTC().Truncate(time.Millisecond)
	if at.IsZero() || at.Year() < 1000 || at.Year() > 9999 {
		return model.AppealEligibilitySnapshot{}, fmt.Errorf("%w: evaluation time", ErrInvalidArgument)
	}
	facts, missing, err := NormalizeAppealFacts(raw)
	if err != nil {
		return model.AppealEligibilitySnapshot{}, err
	}

	snapshot := model.AppealEligibilitySnapshot{
		SchemaVersion: model.AppealEligibilitySnapshotVersion,
		Decision:      model.AppealEligibilityManualReview,
		ReasonCode:    "policy_unavailable", MissingFields: missing, CheckedAt: at,
		EvidenceRefs: []model.AppealRuleEvidence{},
		FactSources: []model.AppealFactProvenance{
			{Field: "subject_type", Source: model.AppealFactUserReported},
		},
	}
	if facts.SubjectRef != "" {
		snapshot.FactSources = append(snapshot.FactSources, model.AppealFactProvenance{
			Field: "subject_ref", Source: model.AppealFactUserReported,
		})
	}
	if facts.Reason != "" {
		snapshot.FactSources = append(snapshot.FactSources, model.AppealFactProvenance{
			Field: "reason", Source: model.AppealFactUserReported,
		})
	}
	if len(missing) != 0 {
		snapshot.Decision = model.AppealEligibilityNeedsMoreInfo
		snapshot.ReasonCode = "missing_fields"
		return NormalizeAppealSnapshot(snapshot)
	}

	policy, err := s.policies.Resolve(ctx, facts.SubjectType)
	if err != nil {
		return model.AppealEligibilitySnapshot{}, fmt.Errorf("resolve appeal policy: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return model.AppealEligibilitySnapshot{}, err
	}
	if policy == nil {
		return NormalizeAppealSnapshot(snapshot)
	}

	definition := policy.Definition()
	required, err := appealVerifiedFieldSet(definition.RequiredVerifiedFields)
	if err != nil {
		return model.AppealEligibilitySnapshot{}, err
	}
	switch definition.SubjectType {
	case model.AppealSubjectAccount, model.AppealSubjectContent, model.AppealSubjectOther:
	default:
		return model.AppealEligibilitySnapshot{}, ErrAppealInvalidPolicy
	}
	snapshot.PolicyCode = definition.Code
	snapshot.PolicyRevision = definition.Revision
	snapshot.EvidenceRefs = definition.EvidenceRefs
	snapshot, err = NormalizeAppealSnapshot(snapshot)
	if err != nil || snapshot.PolicyCode == "" {
		return model.AppealEligibilitySnapshot{}, ErrAppealInvalidPolicy
	}
	if definition.SubjectType != facts.SubjectType {
		snapshot.ReasonCode = "policy_scope_mismatch"
		return snapshot, nil
	}
	if evidence == nil {
		return model.AppealEligibilitySnapshot{}, ErrAppealConfiguration
	}
	current, err := evidence.CheckAppealEvidence(ctx,
		append([]model.AppealRuleEvidence{}, snapshot.EvidenceRefs...), at)
	if err != nil {
		return model.AppealEligibilitySnapshot{}, fmt.Errorf("check appeal evidence: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return model.AppealEligibilitySnapshot{}, err
	}
	if !current {
		snapshot.ReasonCode = "policy_evidence_unavailable"
		return snapshot, nil
	}

	assessment, err := policy.Evaluate(ctx, facts, at)
	if err != nil {
		return model.AppealEligibilitySnapshot{}, fmt.Errorf("evaluate appeal policy: %w", err)
	}
	switch assessment.Decision {
	case model.AppealEligibilityEligible, model.AppealEligibilityIneligible, model.AppealEligibilityManualReview:
	default:
		return model.AppealEligibilitySnapshot{}, ErrAppealInvalidPolicy
	}
	if !appealReasonPattern.MatchString(assessment.ReasonCode) {
		return model.AppealEligibilitySnapshot{}, ErrAppealInvalidPolicy
	}
	verified, err := appealVerifiedFieldSet(assessment.VerifiedFields)
	if err != nil {
		return model.AppealEligibilitySnapshot{}, err
	}
	snapshot.Decision = assessment.Decision
	snapshot.ReasonCode = assessment.ReasonCode
	for i := range snapshot.FactSources {
		if verified[snapshot.FactSources[i].Field] {
			snapshot.FactSources[i].Source = model.AppealFactServerVerified
		}
	}
	if snapshot.Decision == model.AppealEligibilityEligible || snapshot.Decision == model.AppealEligibilityIneligible {
		for field := range required {
			if !verified[field] {
				snapshot.Decision = model.AppealEligibilityManualReview
				snapshot.ReasonCode = "facts_unverified"
				break
			}
		}
	}
	return NormalizeAppealSnapshot(snapshot)
}

func appealVerifiedFieldSet(fields []string) (map[string]bool, error) {
	if len(fields) > 3 {
		return nil, ErrAppealInvalidPolicy
	}
	result := make(map[string]bool, len(fields))
	for _, field := range fields {
		if !appealFactField(field) || result[field] {
			return nil, ErrAppealInvalidPolicy
		}
		result[field] = true
	}
	return result, nil
}
