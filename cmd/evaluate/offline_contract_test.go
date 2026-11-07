package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"video-support-agent/internal/evaluation"
)

func offlineSnapshot(t *testing.T) evaluation.ScoreSnapshot {
	t.Helper()
	no := false
	d := strings.Repeat("a", 64)
	c := evaluation.Case{ID: "offline-case", SchemaVersion: 1, Suite: "agent", Question: "合成问题", CorpusID: "synthetic", Filters: &evaluation.Filters{}, TopK: 3, ExpectedIntent: "clarify", ExpectedDocuments: []string{}, ExpectedTools: []evaluation.ToolExpectation{}, ReferenceAnswer: "补充问题", ExpectedCitations: []evaluation.CitationKey{}, ExpectedHandoff: &no, RequiredPoints: []string{"询问事项"}, ForbiddenClaims: []string{"编造工单"}}
	o := evaluation.AgentObservation{CaseID: c.ID, CorpusID: c.CorpusID, CorpusSHA256: d, RunID: "offline-run", Status: evaluation.RunCompleted, Decision: "clarify", Answer: "请补充事项", Tools: []evaluation.ObservedTool{}, Citations: []evaluation.CitationKey{}}
	s, e := evaluation.FreezeScoreSnapshot(evaluation.ScoreSnapshot{SchemaVersion: 1, RunID: o.RunID, Mode: "chat", RunIndex: 1, PlannedCases: 1, Identity: evaluation.EvaluationIdentity{SourceSHA256: d, InputsSHA256: d, CorpusSHA256: d, BindingSHA256: d, ModelConfigSHA256: d, PlannerModel: "synthetic", AnswerModel: "synthetic", EmbeddingModel: "synthetic"}, Cases: []evaluation.Case{c}, Agents: []evaluation.AgentObservation{o}, Retrieval: []evaluation.RetrievalObservation{}})
	if e != nil {
		t.Fatal(e)
	}
	return s
}

func TestOfflineCommandRoundTrip(t *testing.T) {
	dir := t.TempDir()
	snapshotPath := filepath.Join(dir, "snapshot.json")
	reviewPath := filepath.Join(dir, "review.json")
	reportPath := filepath.Join(dir, "report.json")
	digest, e := evaluation.WriteNewJSON(snapshotPath, offlineSnapshot(t))
	if e != nil {
		t.Fatal(e)
	}
	if e = run([]string{"review-draft", "-snapshot", snapshotPath, "-out", reviewPath}); e != nil {
		t.Fatal(e)
	}
	var review evaluation.ReviewDocument
	if _, e = evaluation.ReadStrictJSON(reviewPath, &review); e != nil {
		t.Fatal(e)
	}
	if review.SnapshotFileSHA256 != digest || review.Reviews[0].RequiredPointsMet[0] != nil || review.Reviews[0].ForbiddenClaimsAbsent[0] != nil {
		t.Fatal("draft not bound or pending")
	}
	if e = run([]string{"rescore", "-snapshot", snapshotPath, "-review", reviewPath, "-out", reportPath}); e != nil {
		t.Fatal(e)
	}
	var report evaluation.FrozenReport
	if _, e = evaluation.ReadStrictJSON(reportPath, &report); e != nil {
		t.Fatal(e)
	}
	if report.SnapshotFileSHA256 != digest || len(report.ReviewFileSHA256) != 64 || len(report.ScorerBinarySHA256) != 64 || report.Agent.TotalCases != 1 {
		t.Fatal("report evidence links incomplete")
	}
	before, _ := os.ReadFile(reportPath)
	if e = run([]string{"rescore", "-snapshot", snapshotPath, "-out", reportPath}); e == nil {
		t.Fatal("existing report overwritten")
	}
	after, _ := os.ReadFile(reportPath)
	if string(before) != string(after) {
		t.Fatal("existing report changed")
	}
	review.SnapshotFileSHA256 = strings.Repeat("b", 64)
	wrong := filepath.Join(dir, "wrong.json")
	if _, e = evaluation.WriteNewJSON(wrong, review); e != nil {
		t.Fatal(e)
	}
	if e = run([]string{"rescore", "-snapshot", snapshotPath, "-review", wrong, "-out", filepath.Join(dir, "bad.json")}); e == nil {
		t.Fatal("mismatched review accepted")
	}
	if _, e = os.Stat(filepath.Join(dir, "bad.json")); !os.IsNotExist(e) {
		t.Fatal("invalid review produced output")
	}
}

func TestOfflineCommandRejectsInvalidArguments(t *testing.T) {
	for _, args := range [][]string{nil, {"unknown"}, {"run"}, {"rescore"}, {"review-draft", "-snapshot", "x", "-review", "x", "-out", "y"}, {"rescore", "-snapshot", "x", "-out", "y", "extra"}} {
		if e := run(args); e == nil {
			t.Fatalf("accepted invalid args %v", args)
		}
	}
}

func TestSourceFingerprintDetectsChanges(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"cmd", "internal"} {
		if e := os.Mkdir(filepath.Join(root, dir), 0700); e != nil {
			t.Fatal(e)
		}
	}
	files := map[string]string{"go.mod": "module video-support-agent\n", "go.sum": "", "cmd/main.go": "package main\n", "internal/x.go": "package x\n", "internal/x_test.go": "package x\n"}
	for path, content := range files {
		if e := os.WriteFile(filepath.Join(root, path), []byte(content), 0600); e != nil {
			t.Fatal(e)
		}
	}
	first, e := sourceFingerprint(root)
	if e != nil {
		t.Fatal(e)
	}
	h1, _ := evaluation.EvidenceHash(first)
	if e := os.WriteFile(filepath.Join(root, "internal/x_test.go"), []byte("package x // test changed\n"), 0600); e != nil {
		t.Fatal(e)
	}
	second, e := sourceFingerprint(root)
	if e != nil {
		t.Fatal(e)
	}
	h2, _ := evaluation.EvidenceHash(second)
	if h1 != h2 {
		t.Fatal("test files entered business fingerprint")
	}
	if e := os.WriteFile(filepath.Join(root, "internal/x.go"), []byte("package x // business changed\n"), 0600); e != nil {
		t.Fatal(e)
	}
	third, e := sourceFingerprint(root)
	if e != nil {
		t.Fatal(e)
	}
	h3, _ := evaluation.EvidenceHash(third)
	if h1 == h3 {
		t.Fatal("business change not detected")
	}
}
