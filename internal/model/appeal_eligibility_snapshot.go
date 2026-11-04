package model

import "time"

const AppealEligibilitySnapshotVersion uint16 = 1

type AppealEligibilityDecision string

const (
	AppealEligibilityEligible      AppealEligibilityDecision = "eligible"
	AppealEligibilityIneligible    AppealEligibilityDecision = "ineligible"
	AppealEligibilityNeedsMoreInfo AppealEligibilityDecision = "needs_more_info"
	AppealEligibilityManualReview  AppealEligibilityDecision = "manual_review"
)

type AppealFactSource string

const (
	AppealFactUserReported   AppealFactSource = "user_reported"
	AppealFactServerVerified AppealFactSource = "server_verified"
)

type AppealRuleEvidence struct {
	DocumentID          uint64 `json:"document_id"`
	VersionID           uint64 `json:"version_id"`
	ArticleID           uint64 `json:"article_id"`
	PublicationRevision uint64 `json:"publication_revision"`
}

type AppealFactProvenance struct {
	Field  string           `json:"field"`
	Source AppealFactSource `json:"source"`
}

type AppealEligibilitySnapshot struct {
	SchemaVersion  uint16                    `json:"schema_version"`
	Decision       AppealEligibilityDecision `json:"decision"`
	ReasonCode     string                    `json:"reason_code"`
	MissingFields  []string                  `json:"missing_fields"`
	CheckedAt      time.Time                 `json:"checked_at"`
	PolicyCode     string                    `json:"policy_code"`
	PolicyRevision string                    `json:"policy_revision"`
	EvidenceRefs   []AppealRuleEvidence      `json:"evidence_refs"`
	FactSources    []AppealFactProvenance    `json:"fact_sources"`
}
