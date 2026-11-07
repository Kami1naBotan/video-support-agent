package evaluation

import (
	"fmt"
	"unicode/utf8"
)

func validateAgentObservation(c Case, o AgentObservation) error {
	if o.CaseID != c.ID || o.CorpusID != c.CorpusID {
		return fieldError("observation", "case or corpus mismatch")
	}
	if err := validateKey("run_id", o.RunID); err != nil {
		return err
	}
	if !validDigest(o.CorpusSHA256) {
		return fieldError("corpus_sha256", "must be a SHA-256 digest")
	}
	if !oneOf(o.Status, RunCompleted, RunFailed, RunCancelled, RunTimedOut, RunUnknown) {
		return fieldError("status", "unsupported run status")
	}
	if !oneOf(o.Decision, "answer", "clarify", "handoff", "confirmation_required", "ticket_created", "ticket_status", "failed") {
		return fieldError("decision", "unsupported decision")
	}
	if !utf8.ValidString(o.Answer) || len(o.Answer) > MaxReferenceBytes {
		return fieldError("answer", "invalid encoding or size")
	}
	if o.Tools == nil || len(o.Tools) > 100 {
		return fieldError("tools", "invalid array or size")
	}
	if o.Citations == nil || len(o.Citations) > MaxObservedItems {
		return fieldError("citations", "invalid array or size")
	}

	for i, tool := range o.Tools {
		path := fmt.Sprintf("tools[%d]", i)
		if err := validateKey(path+".name", tool.Name); err != nil {
			return err
		}
		if !oneOf(tool.Status, "succeeded", "rejected", "failed", "cancelled", "timed_out", "unknown") {
			return fieldError(path+".status", "unsupported status")
		}
		if !oneOf(tool.BusinessEffect, "none", "unknown", "committed") {
			return fieldError(path+".business_effect", "unsupported effect")
		}
		if err := validateArguments(tool.Arguments); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	for i, key := range o.Citations {
		for _, value := range []string{key.DocumentKey, key.VersionKey, key.ArticleKey} {
			if err := validateKey(fmt.Sprintf("citations[%d]", i), value); err != nil {
				return err
			}
		}
	}
	return nil
}

func ScoreAgentCase(c Case, o AgentObservation, review *AgentReview) (AgentCaseScore, error) {
	if err := ValidateCase(c); err != nil {
		return AgentCaseScore{}, err
	}
	if c.Suite != SuiteAgent {
		return AgentCaseScore{}, fieldError("suite", "requires an agent case")
	}
	for _, tool := range c.ExpectedTools {
		if err := validateArguments(tool.ArgumentsMatch); err != nil {
			return AgentCaseScore{}, err
		}
	}
	if err := validateAgentObservation(c, o); err != nil {
		return AgentCaseScore{}, err
	}

	caseHash, err := EvidenceHash(c)
	if err != nil {
		return AgentCaseScore{}, err
	}
	observationHash, err := EvidenceHash(o)
	if err != nil {
		return AgentCaseScore{}, err
	}

	row := AgentCaseScore{
		CaseID:                c.ID,
		CaseSHA256:            caseHash,
		ObservationSHA256:     observationHash,
		Failed:                o.Status != RunCompleted,
		ExpectedTools:         len(c.ExpectedTools) > 0,
		ExpectedHandoff:       *c.ExpectedHandoff,
		PredictedHandoff:      o.Status == RunCompleted && o.Decision == "handoff",
		RequiredPointsMet:     make([]*bool, len(c.RequiredPoints)),
		ForbiddenClaimsAbsent: make([]*bool, len(c.ForbiddenClaims)),
	}

	if !row.Failed {
		row.DecisionMatch = o.Decision == c.ExpectedIntent
		row.ToolSelectionMatch = len(c.ExpectedTools) == len(o.Tools)
		row.ToolMatch = row.ToolSelectionMatch

		if row.ToolSelectionMatch {
			for i, expected := range c.ExpectedTools {
				actual := o.Tools[i]
				if expected.Name != actual.Name {
					row.ToolSelectionMatch = false
					row.ToolMatch = false
				}
				if expected.ExpectedStatus != actual.Status ||
					expected.ExpectedBusinessEffect != actual.BusinessEffect ||
					!argumentsMatch(expected.ArgumentsMatch, actual.Arguments) {
					row.ToolMatch = false
				}
			}
		}
	}

	actualCitations := make(map[CitationKey]struct{}, len(o.Citations))
	for _, key := range o.Citations {
		actualCitations[key] = struct{}{}
	}
	expectedCitations := make(map[CitationKey]struct{}, len(c.ExpectedCitations))
	for _, key := range c.ExpectedCitations {
		expectedCitations[key] = struct{}{}
		if _, exists := actualCitations[key]; !exists {
			row.MissingCitations++
		}
	}

	row.CitationCount = len(actualCitations)
	row.ExpectedCitationCount = len(expectedCitations)
	for key := range actualCitations {
		if _, exists := expectedCitations[key]; exists {
			row.IdentityMatches++
		}
	}

	reviews, err := bindAgentReview(c, row, actualCitations, review)
	if err != nil {
		return AgentCaseScore{}, err
	}
	if review != nil {
		copy(row.RequiredPointsMet, review.RequiredPointsMet)
		copy(row.ForbiddenClaimsAbsent, review.ForbiddenClaimsAbsent)
	}

	for key := range actualCitations {
		item, exists := reviews[key]
		if !exists || item.ValidIdentityAndScope == nil || item.SupportsAllClaims == nil {
			row.PendingCitations++
			continue
		}
		if *item.ValidIdentityAndScope && *item.SupportsAllClaims {
			row.SemanticCorrect++
		}
	}
	return row, nil
}

func bindAgentReview(c Case, row AgentCaseScore, actual map[CitationKey]struct{}, review *AgentReview) (map[CitationKey]CitationReview, error) {
	result := make(map[CitationKey]CitationReview)
	if review == nil {
		return result, nil
	}
	if review.CaseID != c.ID || review.CaseSHA256 != row.CaseSHA256 || review.ObservationSHA256 != row.ObservationSHA256 {
		return nil, fieldError("review", "content digest or case mismatch")
	}
	if review.Citations == nil || len(review.Citations) > len(actual) {
		return nil, fieldError("review.citations", "invalid array or size")
	}
	if review.RequiredPointsMet == nil ||
		len(review.RequiredPointsMet) != len(c.RequiredPoints) ||
		review.ForbiddenClaimsAbsent == nil ||
		len(review.ForbiddenClaimsAbsent) != len(c.ForbiddenClaims) {
		return nil, fieldError("review", "checklist lengths do not match")
	}
	if err := validateText("review.evidence", review.Evidence, 4096); err != nil {
		return nil, err
	}

	for _, item := range review.Citations {
		if _, exists := actual[item.Key]; !exists {
			return nil, fieldError("review.citations", "citation was not observed")
		}
		if _, exists := result[item.Key]; exists {
			return nil, fieldError("review.citations", "duplicate review")
		}
		if err := validateText("review.citations.evidence", item.Evidence, 4096); err != nil {
			return nil, err
		}
		result[item.Key] = item
	}
	return result, nil
}
