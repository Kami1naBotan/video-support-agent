package evaluation

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func contractCase(id string) Case {
	no := false
	return Case{ID: id, SchemaVersion: SchemaVersion, Suite: SuiteAgent, Question: "合成问题", CorpusID: "synthetic", Filters: &Filters{}, TopK: 3, ExpectedIntent: "clarify", ExpectedDocuments: []string{}, ExpectedTools: []ToolExpectation{}, ReferenceAnswer: "请补充事项", ExpectedCitations: []CitationKey{}, ExpectedHandoff: &no, RequiredPoints: []string{}, ForbiddenClaims: []string{}}
}
func contractObservation(c Case) AgentObservation {
	return AgentObservation{CaseID: c.ID, CorpusID: c.CorpusID, CorpusSHA256: strings.Repeat("a", 64), RunID: "contract-run", Status: RunCompleted, Decision: c.ExpectedIntent, Answer: "合成回答", Tools: []ObservedTool{}, Citations: []CitationKey{}}
}
func assertMetric(t *testing.T, actual *float64, wanted float64) {
	t.Helper()
	if actual == nil || math.Abs(*actual-wanted) > 1e-12 {
		t.Fatalf("metric=%v, want %v", actual, wanted)
	}
}
func citation(doc, article string) CitationKey {
	return CitationKey{DocumentKey: doc, VersionKey: "v1", ArticleKey: article}
}

func TestRetrievalMetricContract(t *testing.T) {
	makeCase := func(id string, docs ...string) Case {
		c := contractCase(id)
		c.Suite = SuiteRetrieval
		c.ExpectedIntent = "retrieve"
		c.ExpectedHandoff = nil
		c.ExpectedDocuments = append([]string{}, docs...)
		return c
	}
	cases := []Case{makeCase("positive", "a", "b"), makeCase("failed", "a"), makeCase("negative")}
	observed := []RetrievalObservation{
		{CaseID: "positive", CorpusID: "synthetic", TopK: 3, Status: RunCompleted, Items: []CitationKey{citation("x", "1"), citation("a", "2"), citation("a", "3"), citation("b", "4")}},
		{CaseID: "failed", CorpusID: "synthetic", TopK: 3, Status: RunTimedOut, Items: []CitationKey{}},
		{CaseID: "negative", CorpusID: "synthetic", TopK: 3, Status: RunCompleted, Items: []CitationKey{citation("x", "1")}},
	}
	report, err := ScoreRetrieval(cases, observed)
	if err != nil {
		t.Fatal(err)
	}
	assertMetric(t, report.RecallAtK, .25)
	assertMetric(t, report.MRRAtK, .25)
	if report.ApplicableCases != 2 || report.NegativeCases != 1 || report.FailedCases != 1 || report.NegativeWithResults != 1 {
		t.Fatalf("wrong denominators: %+v", report)
	}
	if report.Cases[0].FirstRelevantRank != 2 || report.Cases[0].HitDocuments != 1 {
		t.Fatal("ranking was deduplicated or refilled")
	}
	t.Run("negative_only", func(t *testing.T) {
		r, e := ScoreRetrieval(cases[2:], observed[2:])
		if e != nil || r.RecallAtK != nil || r.MRRAtK != nil {
			t.Fatalf("N/A not preserved: %+v %v", r, e)
		}
	})
	for name, rows := range map[string][]RetrievalObservation{"missing": observed[:2], "duplicate": {observed[0], observed[0], observed[2]}} {
		t.Run(name, func(t *testing.T) {
			if _, e := ScoreRetrieval(cases, rows); e == nil {
				t.Fatal("invalid identity alignment accepted")
			}
		})
	}
}

func TestAgentHandoffFailureDenominators(t *testing.T) {
	yes := true
	cases := []Case{contractCase("tp"), contractCase("tn"), contractCase("failed-positive"), contractCase("failed-negative")}
	for _, i := range []int{0, 2} {
		cases[i].ExpectedIntent = "handoff"
		cases[i].ExpectedHandoff = &yes
	}
	obs := make([]AgentObservation, len(cases))
	for i, c := range cases {
		obs[i] = contractObservation(c)
	}
	obs[2].Status = RunFailed
	obs[3].Status = RunTimedOut
	report, err := ScoreAgents(cases, obs, nil)
	if err != nil {
		t.Fatal(err)
	}
	if report.Handoff.TP != 1 || report.Handoff.TN != 1 || report.Handoff.FailedPositive != 1 || report.Handoff.FailedNegative != 1 || report.Handoff.FN != 0 {
		t.Fatalf("failure confused with classifier decision: %+v", report.Handoff)
	}
	assertMetric(t, report.HandoffAccuracy.Value, .5)
	assertMetric(t, report.HandoffRecall.Value, .5)
	assertMetric(t, report.ToolAccuracy.Value, .5)
	if report.WithToolsAccuracy.Value != nil || report.WithoutToolsAccuracy.Total != 4 {
		t.Fatal("tool strata denominators incorrect")
	}
}

func TestToolArgumentsAndExecutionContract(t *testing.T) {
	c := contractCase("tool")
	c.ExpectedTools = []ToolExpectation{{Name: "search_rule", ArgumentsMatch: map[string]json.RawMessage{"top_k": json.RawMessage(`1.0`)}, ExpectedStatus: "succeeded", ExpectedBusinessEffect: "none"}}
	original := contractObservation(c)
	original.Tools = []ObservedTool{{Name: "search_rule", Arguments: map[string]json.RawMessage{"top_k": json.RawMessage(`1`), "query": json.RawMessage(`"合成"`)}, Status: "succeeded", BusinessEffect: "none"}}
	for _, tc := range []struct {
		name   string
		change func(*AgentObservation)
		want   bool
	}{
		{"numeric_equivalence", func(*AgentObservation) {}, true},
		{"wrong_argument", func(o *AgentObservation) {
			o.Tools[0].Arguments = map[string]json.RawMessage{"top_k": json.RawMessage(`2`)}
		}, false},
		{"wrong_effect", func(o *AgentObservation) { o.Tools[0].BusinessEffect = "unknown" }, false},
		{"extra_call", func(o *AgentObservation) { o.Tools = append(o.Tools, o.Tools[0]) }, false},
		{"failed_run", func(o *AgentObservation) { o.Status = RunFailed }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := original
			o.Tools = append([]ObservedTool{}, original.Tools...)
			tc.change(&o)
			row, e := ScoreAgentCase(c, o, nil)
			if e != nil {
				t.Fatal(e)
			}
			if row.ToolMatch != tc.want {
				t.Fatalf("match=%v", row.ToolMatch)
			}
		})
	}
	if argumentsMatch(map[string]json.RawMessage{"n": json.RawMessage(`9007199254740993`)}, map[string]json.RawMessage{"n": json.RawMessage(`9007199254740992`)}) {
		t.Fatal("large integers rounded together")
	}
	if _, err := argumentValue(json.RawMessage(`{"x":1,"x":2}`)); err == nil {
		t.Fatal("duplicate argument key accepted")
	}
}

func TestCitationReviewPendingAndBinding(t *testing.T) {
	c := contractCase("citation")
	c.ExpectedDocuments = []string{"a"}
	c.ExpectedCitations = []CitationKey{citation("a", "1")}
	o := contractObservation(c)
	o.Citations = []CitationKey{citation("a", "1"), citation("a", "1"), citation("b", "2")}
	pending, e := ScoreAgents([]Case{c}, []AgentObservation{o}, nil)
	if e != nil {
		t.Fatal(e)
	}
	if pending.CitationSemanticAccuracy.Total != 2 || pending.CitationSemanticAccuracy.Pending != 2 || pending.CitationSemanticAccuracy.Value != nil {
		t.Fatal("pending references were discarded or duplicates counted")
	}
	assertMetric(t, pending.CitationIdentityPrecision.Value, .5)
	ch, _ := EvidenceHash(c)
	oh, _ := EvidenceHash(o)
	yes, no := true, false
	review := AgentReview{CaseID: c.ID, CaseSHA256: ch, ObservationSHA256: oh, Citations: []CitationReview{{Key: citation("a", "1"), ValidIdentityAndScope: &yes, SupportsAllClaims: &yes, Evidence: "合成条文逐项支持"}, {Key: citation("b", "2"), ValidIdentityAndScope: &no, SupportsAllClaims: &no, Evidence: "不属于预期依据"}}, RequiredPointsMet: []*bool{}, ForbiddenClaimsAbsent: []*bool{}, Evidence: "逐条审核"}
	report, e := ScoreAgents([]Case{c}, []AgentObservation{o}, []AgentReview{review})
	if e != nil {
		t.Fatal(e)
	}
	assertMetric(t, report.CitationSemanticAccuracy.Value, .5)
	o.Answer = "changed"
	if _, e = ScoreAgents([]Case{c}, []AgentObservation{o}, []AgentReview{review}); e == nil {
		t.Fatal("stale review accepted")
	}
	o.Citations = []CitationKey{}
	report, e = ScoreAgents([]Case{c}, []AgentObservation{o}, nil)
	if e != nil {
		t.Fatal(e)
	}
	if report.MissingCitationCases != 1 || report.CitationSemanticAccuracy.Value != nil {
		t.Fatal("missing citation treated as semantic success")
	}
}
