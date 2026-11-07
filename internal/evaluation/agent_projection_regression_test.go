package evaluation

import (
	"context"
	"encoding/json"
	"runtime"
	"strings"
	"testing"

	"video-support-agent/internal/model"
	"video-support-agent/internal/service"
)

func projectionFixture() (*AgentRunner, Case) {
	handoff := false
	c := Case{ID: "projection-case", SchemaVersion: SchemaVersion, Suite: SuiteAgent,
		Question: "查询 $ticket_id", CorpusID: "synthetic-corpus", Filters: &Filters{}, TopK: 3,
		ExpectedIntent: "clarify", ExpectedDocuments: []string{}, ExpectedTools: []ToolExpectation{},
		ReferenceAnswer: "请补充具体问题。", ExpectedCitations: []CitationKey{}, ExpectedHandoff: &handoff,
		RequiredPoints: []string{}, ForbiddenClaims: []string{}}
	r := &AgentRunner{binding: &CorpusBinding{corpus: &Corpus{id: c.CorpusID, digest: strings.Repeat("a", 64), synthetic: true}}}
	return r, c
}

func TestResolveTicketCasePreservesEmptyArray(t *testing.T) {
	_, c := projectionFixture()
	if err := ValidateCase(c); err != nil {
		t.Fatal(err)
	}
	resolved := resolveTicketCase(c, 42)
	if err := ValidateCase(resolved); err != nil {
		t.Fatalf("valid case became invalid: %v", err)
	}
	raw, err := json.Marshal(resolved)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"expected_tools":[]`) {
		t.Fatal("empty tools must remain an explicit array")
	}
	if c.Question != "查询 $ticket_id" {
		t.Fatal("original case changed")
	}
}

func TestProjectAttemptPreservesFailureClassification(t *testing.T) {
	for _, tc := range []struct {
		name, status, effect string
		runErr               error
	}{
		{"unknown", RunUnknown, "unknown", service.ErrAppealOutcomeUnknown},
		{"timeout", RunTimedOut, "none", context.DeadlineExceeded},
		{"cancel", RunCancelled, "none", context.Canceled},
		{"completed_invalid", RunFailed, "none", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, c := projectionFixture()
			a := r.emptyAttempt(c, "batch-run")
			out := &service.AgentOutcome{RunID: "11111111-1111-4111-8111-111111111111", ExecutionStatus: RunCompleted, Decision: "clarify", BusinessEffect: tc.effect,
				Citations: []model.RuleCitation{{DocumentID: "bad-id"}}}
			if tc.name == "unknown" {
				out.ExecutionStatus = RunUnknown
			}
			r.projectAttempt(&a, c, out, []ObservedTool{}, tc.runErr, nil)
			if a.Observation.Status != tc.status {
				t.Errorf("status=%s, want %s", a.Observation.Status, tc.status)
			}
			if a.BusinessEffect != tc.effect {
				t.Errorf("effect=%s, want %s", a.BusinessEffect, tc.effect)
			}
			if a.ErrorCode != "invalid_observation" {
				t.Errorf("missing projection diagnostic: %s", a.ErrorCode)
			}
			if err := validateAgentObservation(c, a.Observation); err != nil {
				t.Errorf("fallback cannot be scored: %v", err)
			}
		})
	}
}

func TestProjectAttemptRejectsOversizedCitationsWithinAllocationBudget(t *testing.T) {
	r, c := projectionFixture()
	a := r.emptyAttempt(c, "batch-run")
	out := &service.AgentOutcome{ExecutionStatus: RunCompleted, Decision: "clarify", BusinessEffect: "none", Citations: make([]model.RuleCitation, 100000)}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	r.projectAttempt(&a, c, out, []ObservedTool{}, nil, nil)
	runtime.ReadMemStats(&after)
	allocated := after.TotalAlloc - before.TotalAlloc
	if a.ErrorCode != "invalid_observation" || a.Observation.Status != RunFailed {
		t.Fatal("oversized result not explicitly rejected")
	}
	const budget = 1 << 20
	if allocated > budget {
		t.Fatalf("oversized citations allocated %d bytes; budget %d", allocated, budget)
	}
	runtime.KeepAlive(out)
}
