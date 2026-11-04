package service

import (
	"encoding/json"
	"regexp"
	"sort"
	"time"

	"video-support-agent/internal/model"
)

var (
	appealReasonPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	appealPolicyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
)

func appealFactField(field string) bool {
	return field == "subject_type" || field == "subject_ref" || field == "reason"
}

func NormalizeAppealSnapshot(raw model.AppealEligibilitySnapshot) (model.AppealEligibilitySnapshot, error) {
	bad := func() (model.AppealEligibilitySnapshot, error) {
		return model.AppealEligibilitySnapshot{}, ErrAppealInvalidSnapshot
	}
	if raw.SchemaVersion != model.AppealEligibilitySnapshotVersion ||
		raw.CheckedAt.IsZero() || raw.CheckedAt.UTC().Year() < 1000 || raw.CheckedAt.UTC().Year() > 9999 ||
		!appealReasonPattern.MatchString(raw.ReasonCode) ||
		len(raw.MissingFields) > 2 || len(raw.EvidenceRefs) > 8 || len(raw.FactSources) > 3 {
		return bad()
	}

	s := raw
	s.CheckedAt = raw.CheckedAt.UTC().Truncate(time.Millisecond)
	s.MissingFields = append([]string{}, raw.MissingFields...)
	s.EvidenceRefs = append([]model.AppealRuleEvidence{}, raw.EvidenceRefs...)
	s.FactSources = append([]model.AppealFactProvenance{}, raw.FactSources...)

	missing := make(map[string]bool, len(s.MissingFields))
	for _, field := range s.MissingFields {
		if (field != "subject_ref" && field != "reason") || missing[field] {
			return bad()
		}
		missing[field] = true
	}
	noPolicy := s.PolicyCode == "" && s.PolicyRevision == ""
	if noPolicy {
		if len(s.EvidenceRefs) != 0 {
			return bad()
		}
	} else if !appealPolicyPattern.MatchString(s.PolicyCode) ||
		!appealPolicyPattern.MatchString(s.PolicyRevision) || len(s.EvidenceRefs) == 0 {
		return bad()
	}

	switch s.Decision {
	case model.AppealEligibilityEligible, model.AppealEligibilityIneligible:
		if noPolicy || len(missing) != 0 {
			return bad()
		}
	case model.AppealEligibilityNeedsMoreInfo:
		if len(missing) == 0 {
			return bad()
		}
	case model.AppealEligibilityManualReview:
		if len(missing) != 0 {
			return bad()
		}
	default:
		return bad()
	}

	seenFields := make(map[string]bool, len(s.FactSources))
	for _, fact := range s.FactSources {
		if !appealFactField(fact.Field) || seenFields[fact.Field] || missing[fact.Field] {
			return bad()
		}
		if fact.Source != model.AppealFactUserReported && fact.Source != model.AppealFactServerVerified {
			return bad()
		}
		seenFields[fact.Field] = true
	}
	if len(seenFields) != 3-len(missing) || !seenFields["subject_type"] {
		return bad()
	}

	seenArticles := make(map[uint64]bool, len(s.EvidenceRefs))
	documentVersions := make(map[uint64][2]uint64)
	for _, ref := range s.EvidenceRefs {
		if ref.DocumentID == 0 || ref.VersionID == 0 || ref.ArticleID == 0 ||
			ref.PublicationRevision == 0 || seenArticles[ref.ArticleID] {
			return bad()
		}
		identity := [2]uint64{ref.VersionID, ref.PublicationRevision}
		if previous, exists := documentVersions[ref.DocumentID]; exists && previous != identity {
			return bad()
		}
		documentVersions[ref.DocumentID] = identity
		seenArticles[ref.ArticleID] = true
	}

	sort.Strings(s.MissingFields)
	sort.Slice(s.FactSources, func(i, j int) bool {
		return s.FactSources[i].Field < s.FactSources[j].Field
	})
	sort.Slice(s.EvidenceRefs, func(i, j int) bool {
		a, b := s.EvidenceRefs[i], s.EvidenceRefs[j]
		left := [4]uint64{a.DocumentID, a.VersionID, a.ArticleID, a.PublicationRevision}
		right := [4]uint64{b.DocumentID, b.VersionID, b.ArticleID, b.PublicationRevision}
		for k := range left {
			if left[k] != right[k] {
				return left[k] < right[k]
			}
		}
		return false
	})
	return s, nil
}

func AppealPolicyDigest(raw model.AppealEligibilitySnapshot) (string, error) {
	s, err := NormalizeAppealSnapshot(raw)
	if err != nil {
		return "", err
	}
	stable := struct {
		SchemaVersion  uint16                          `json:"schema_version"`
		Decision       model.AppealEligibilityDecision `json:"decision"`
		ReasonCode     string                          `json:"reason_code"`
		MissingFields  []string                        `json:"missing_fields"`
		PolicyCode     string                          `json:"policy_code"`
		PolicyRevision string                          `json:"policy_revision"`
		EvidenceRefs   []model.AppealRuleEvidence      `json:"evidence_refs"`
		FactSources    []model.AppealFactProvenance    `json:"fact_sources"`
	}{s.SchemaVersion, s.Decision, s.ReasonCode, s.MissingFields,
		s.PolicyCode, s.PolicyRevision, s.EvidenceRefs, s.FactSources}
	data, err := json.Marshal(stable)
	if err != nil {
		return "", err
	}
	return appealSHA256(data), nil
}

func AppealInitialStatus(raw model.AppealEligibilitySnapshot, mode model.AppealIntakeMode) (model.AppealTicketStatus, error) {
	s, err := NormalizeAppealSnapshot(raw)
	if err != nil {
		return "", err
	}
	if mode != model.AppealIntakeStandard && mode != model.AppealIntakeManualReview {
		return "", ErrInvalidArgument
	}
	switch s.Decision {
	case model.AppealEligibilityNeedsMoreInfo:
		return "", ErrAppealNeedsMoreInfo
	case model.AppealEligibilityIneligible:
		return "", ErrAppealIneligible
	case model.AppealEligibilityEligible:
		if mode == model.AppealIntakeStandard {
			return model.AppealTicketSubmitted, nil
		}
	case model.AppealEligibilityManualReview:
		if mode == model.AppealIntakeManualReview {
			return model.AppealTicketPendingReview, nil
		}
	}
	return "", ErrAppealPreconditionChanged
}
