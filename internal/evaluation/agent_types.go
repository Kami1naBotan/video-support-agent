package evaluation

import "encoding/json"

type ObservedTool struct {
	Name           string                     `json:"name"`
	Arguments      map[string]json.RawMessage `json:"arguments"`
	Status         string                     `json:"status"`
	BusinessEffect string                     `json:"business_effect"`
}

type AgentObservation struct {
	CaseID       string         `json:"case_id"`
	CorpusID     string         `json:"corpus_id"`
	CorpusSHA256 string         `json:"corpus_sha256"`
	RunID        string         `json:"run_id"`
	Status       string         `json:"status"`
	Decision     string         `json:"decision"`
	Answer       string         `json:"answer"`
	Tools        []ObservedTool `json:"tools"`
	Citations    []CitationKey  `json:"citations"`
}

type CitationReview struct {
	Key                   CitationKey `json:"key"`
	ValidIdentityAndScope *bool       `json:"valid_identity_and_scope"`
	SupportsAllClaims     *bool       `json:"supports_all_claims"`
	Evidence              string      `json:"evidence"`
}

type AgentReview struct {
	CaseID                string           `json:"case_id"`
	CaseSHA256            string           `json:"case_sha256"`
	ObservationSHA256     string           `json:"observation_sha256"`
	Citations             []CitationReview `json:"citations"`
	RequiredPointsMet     []*bool          `json:"required_points_met"`
	ForbiddenClaimsAbsent []*bool          `json:"forbidden_claims_absent"`
	Evidence              string           `json:"evidence"`
}

type Metric struct {
	Correct int      `json:"correct"`
	Total   int      `json:"total"`
	Pending int      `json:"pending"`
	State   string   `json:"state"`
	Value   *float64 `json:"value"`
}

type AgentCaseScore struct {
	CaseID                string  `json:"case_id"`
	CaseSHA256            string  `json:"case_sha256"`
	ObservationSHA256     string  `json:"observation_sha256"`
	Failed                bool    `json:"failed"`
	ExpectedTools         bool    `json:"expected_tools"`
	ToolMatch             bool    `json:"tool_match"`
	ToolSelectionMatch    bool    `json:"tool_selection_match"`
	DecisionMatch         bool    `json:"decision_match"`
	ExpectedHandoff       bool    `json:"expected_handoff"`
	PredictedHandoff      bool    `json:"predicted_handoff"`
	CitationCount         int     `json:"citation_count"`
	IdentityMatches       int     `json:"identity_matches"`
	SemanticCorrect       int     `json:"semantic_correct"`
	PendingCitations      int     `json:"pending_citations"`
	ExpectedCitationCount int     `json:"expected_citation_count"`
	MissingCitations      int     `json:"missing_citations"`
	RequiredPointsMet     []*bool `json:"required_points_met"`
	ForbiddenClaimsAbsent []*bool `json:"forbidden_claims_absent"`
}

type HandoffCounts struct {
	Positive       int `json:"positive"`
	Negative       int `json:"negative"`
	TP             int `json:"tp"`
	TN             int `json:"tn"`
	FP             int `json:"fp"`
	FN             int `json:"fn"`
	FailedPositive int `json:"failed_positive"`
	FailedNegative int `json:"failed_negative"`
}

type AgentReport struct {
	RunID                     string           `json:"run_id"`
	TotalCases                int              `json:"total_cases"`
	FailedCases               int              `json:"failed_cases"`
	MissingCitationCases      int              `json:"missing_citation_cases"`
	ToolAccuracy              Metric           `json:"tool_accuracy"`
	WithToolsAccuracy         Metric           `json:"with_tools_accuracy"`
	WithoutToolsAccuracy      Metric           `json:"without_tools_accuracy"`
	ToolSelectionAccuracy     Metric           `json:"tool_selection_accuracy"`
	DecisionAccuracy          Metric           `json:"decision_accuracy"`
	Handoff                   HandoffCounts    `json:"handoff"`
	HandoffAccuracy           Metric           `json:"handoff_accuracy"`
	HandoffPrecision          Metric           `json:"handoff_precision"`
	HandoffRecall             Metric           `json:"handoff_recall"`
	CitationIdentityPrecision Metric           `json:"citation_identity_precision"`
	CitationSemanticAccuracy  Metric           `json:"citation_semantic_accuracy"`
	ExpectedCitationCoverage  Metric           `json:"expected_citation_coverage"`
	Cases                     []AgentCaseScore `json:"cases"`
}
