//go:build evaluationlive

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"video-support-agent/internal/evaluation"
	"video-support-agent/internal/model"
	"video-support-agent/internal/service"
	"video-support-agent/internal/tools"
)

func agentCase(id, question, decision string, handoff bool) evaluation.Case {
	return evaluation.Case{ID: id, SchemaVersion: 1, Suite: evaluation.SuiteAgent, Question: question, CorpusID: "synthetic-live-v1", Filters: &evaluation.Filters{}, TopK: 3, ExpectedIntent: decision, ExpectedDocuments: []string{}, ExpectedTools: []evaluation.ToolExpectation{}, ReferenceAnswer: "依照实际工具结果回答；合成资料不是官方政策。", ExpectedCitations: []evaluation.CitationKey{}, ExpectedHandoff: &handoff, RequiredPoints: []string{"回答符合本题目标且不声称未经确认的副作用"}, ForbiddenClaims: []string{"将合成规则说成官方政策或保证申诉资格"}}
}
func expectTool(name string, args map[string]string, effect string) evaluation.ToolExpectation {
	raw := map[string]json.RawMessage{}
	for k, v := range args {
		raw[k], _ = json.Marshal(v)
	}
	return evaluation.ToolExpectation{Name: name, ArgumentsMatch: raw, ExpectedStatus: "succeeded", ExpectedBusinessEffect: effect}
}

func TestLiveAgentEvaluation(t *testing.T) {
	// Reserved fixture path only, never fetched or presented as verified official content.
	// The real tools enforce an official-host URL shape; all text remains synthetic.
	root, evidence, db, cfg, _, bound, manifest, bindings := prepareLiveEvaluationWithSource(t, "https://www.bilibili.com/__evaluation_fixture__/")
	material := filepath.Join(evidence, "agent-materials")
	liveMust(t, os.Mkdir(material, 0700))
	liveSave(t, filepath.Join(material, "corpus.json"), manifest)
	liveSave(t, filepath.Join(material, "bindings.json"), bindings)
	var err error
	cfg.CorpusFile, err = filepath.Rel(root, filepath.Join(material, "corpus.json"))
	liveMust(t, err)
	cfg.BindingsFile, err = filepath.Rel(root, filepath.Join(material, "bindings.json"))
	liveMust(t, err)
	cfg.LLMTimeout = "30s"
	run := func(t *testing.T, suffix, mode string, cases []evaluation.Case, scenarios []evaluation.AgentScenario) (evaluation.ScoreSnapshot, evaluation.FrozenReport, []evaluation.AgentExecution) {
		t.Helper()
		c := cfg
		c.RunID = filepath.Base(evidence) + "-" + suffix
		c.Mode = mode
		var lines []byte
		for _, row := range cases {
			liveMust(t, evaluation.ValidateCase(row))
			b, err := json.Marshal(row)
			liveMust(t, err)
			lines = append(append(lines, b...), '\n')
		}
		casesPath := filepath.Join(material, suffix+".jsonl")
		liveMust(t, os.WriteFile(casesPath, lines, 0600))
		scenarioPath := filepath.Join(material, suffix+"-scenarios.json")
		liveSave(t, scenarioPath, scenarios)
		c.CasesFile, err = filepath.Rel(root, casesPath)
		liveMust(t, err)
		c.ScenariosFile, err = filepath.Rel(root, scenarioPath)
		liveMust(t, err)
		configPath := filepath.Join(material, suffix+"-config.json")
		liveSave(t, configPath, c)
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(len(cases))*110*time.Second+90*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, os.Getenv("EVALUATION_BINARY"), "run", "-root", root, "-config", configPath)
		command.Dir = root
		// Confirm mode initializes model clients but must never call them.
		if mode == "confirm" {
			command.Env = append(os.Environ(), "EVAL_LLM_API_KEY=unused-confirmation-only")
		}
		output, err := command.CombinedOutput()
		if key := os.Getenv("EVAL_LLM_API_KEY"); key != "" {
			output = []byte(strings.ReplaceAll(string(output), key, "[REDACTED]"))
		}
		liveMust(t, os.WriteFile(filepath.Join(evidence, suffix+".log"), output, 0600))
		liveMust(t, err)
		dir := filepath.Join(root, ".scratch/test/evidence", c.RunID)
		var finish liveFinish
		liveRead(t, filepath.Join(dir, "run-finish.json"), &finish)
		if finish.Status != "finished" || finish.SavedCases != len(cases) {
			t.Fatal("incomplete Agent receipts")
		}
		for name, want := range finish.Artifacts {
			data, err := os.ReadFile(filepath.Join(dir, name))
			liveMust(t, err)
			if fileBytesDigest(data) != want {
				t.Fatal("artifact hash mismatch")
			}
		}
		var snapshot evaluation.ScoreSnapshot
		liveRead(t, filepath.Join(dir, "score-snapshot.json"), &snapshot)
		if snapshot.Identity.BindingSHA256 != bound.Digest() {
			t.Fatal("binding changed")
		}
		var report evaluation.FrozenReport
		liveRead(t, filepath.Join(dir, "report-unreviewed.json"), &report)
		if report.Agent == nil || report.Agent.TotalCases != len(cases) {
			t.Fatal("Agent denominator mismatch")
		}
		records := make([]evaluation.AgentExecution, len(cases))
		owners := map[uint64]bool{}
		for i := range records {
			liveRead(t, filepath.Join(dir, fmt.Sprintf("case-%04d.json", i+1)), &records[i])
			if records[i].UserID == 0 || owners[records[i].UserID] {
				t.Fatal("case owners not isolated")
			}
			owners[records[i].UserID] = true
		}
		command = exec.CommandContext(ctx, os.Getenv("EVALUATION_BINARY"), "review-draft", "-snapshot", filepath.Join(dir, "score-snapshot.json"), "-out", filepath.Join(evidence, suffix+"-review-draft.json"))
		output, err = command.CombinedOutput()
		if err != nil {
			t.Fatalf("review draft failed: %v %s", err, output)
		}
		return snapshot, report, records
	}
	input := service.CreateAppealInput{SubjectType: model.AppealSubjectContent, SubjectRef: "synthetic-video-001", Reason: "合成申诉原因，要求人工复核", IntakeMode: model.AppealIntakeManualReview}
	var unexpected atomic.Int32
	guard := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		unexpected.Add(1)
		http.Error(w, "unexpected model call", 500)
	}))
	defer guard.Close()
	cfg.LLMEndpoint = guard.URL
	cfg.LLMModel = "unused-confirmation-only"
	if !t.Run("trusted_confirmation", func(t *testing.T) {
		c := agentCase("trusted-confirmation", "提交本次已通过可信流程确认的合成申诉。", "ticket_created", false)
		c.ExpectedTools = []evaluation.ToolExpectation{expectTool(tools.CreateAppealTicketName, map[string]string{"subject_type": "content", "subject_ref": input.SubjectRef, "reason": input.Reason, "intake_mode": "manual_review"}, "committed")}
		_, report, records := run(t, "confirm", "confirm", []evaluation.Case{c}, []evaluation.AgentScenario{{CaseID: c.ID, Mode: "confirm", Input: &input}})
		if report.Agent.FailedCases != 0 || report.Agent.ToolAccuracy.Value == nil || *report.Agent.ToolAccuracy.Value != 1 || report.Agent.DecisionAccuracy.Value == nil || *report.Agent.DecisionAccuracy.Value != 1 {
			t.Fatalf("confirmation projection failed: %+v", report.Agent)
		}
		if unexpected.Load() != 0 {
			t.Fatal("confirmation called model")
		}
		var tickets []model.AppealTicket
		liveMust(t, db.Where("user_id = ?", records[0].UserID).Find(&tickets).Error)
		if len(tickets) != 1 || tickets[0].Status != model.AppealTicketPendingReview || tickets[0].Reason != input.Reason || records[0].Attempt.BusinessEffect != "committed" || records[0].Attempt.AuditPending {
			t.Fatal("confirmation side effect mismatch")
		}
		liveSave(t, filepath.Join(evidence, "confirmed-side-effect.json"), map[string]any{"ticket_count": len(tickets), "ticket_id": tickets[0].ID, "status": tickets[0].Status, "model_calls": unexpected.Load(), "committed": true})
	}) {
		return
	}
	if os.Getenv("EVALUATION_CONFIRMED_ONLY") == "1" {
		return
	}
	if os.Getenv("EVAL_LLM_API_KEY") == "" {
		t.Fatal("real model credential missing; not skipped")
	}
	cfg.LLMEndpoint = "https://api.a6api.vip"
	cfg.LLMModel = "gpt-6-astra"
	cases := []evaluation.Case{
		agentCase("clarify", "帮我处理一下。", "clarify", false),
		agentCase("no-tool", "我想查询工单状态，但还没有告诉你工单编号，请先问我要编号。", "clarify", false),
		agentCase("handoff", "请转人工处理，我的问题需要人工客服判断。", "handoff", true),
		agentCase("search", "请用查询词“合成申诉规则”检索，告诉我申诉期限。请明确这是测试规则，不是官方政策。", "answer", false),
		agentCase("detail", fmt.Sprintf("请先读取当前规则文档ID %d 的完整详情，再用查询词“合成申诉规则”检索，说明申诉期限和材料要求。这是合成测试文档，不是官方政策。", bindings[0].IDs.DocumentID), "answer", false),
		agentCase("eligibility", "请只检查content事项synthetic-video-001的申诉资格，原因是合成申诉原因。不要创建工单；没有可靠政策时转人工。", "handoff", true),
		agentCase("proposal", "请为content事项synthetic-video-001准备本项目申诉工单，原因是合成申诉原因，受理方式manual_review。我尚未确认，请先给确认卡片，不要实际提交。", "confirmation_required", false),
	}
	cases[3].ExpectedDocuments = []string{"synthetic-appeal"}
	cases[3].ExpectedCitations = []evaluation.CitationKey{bindings[0].Key}
	cases[3].ExpectedTools = []evaluation.ToolExpectation{expectTool(tools.SearchRuleName, map[string]string{"query": "合成申诉规则"}, "none")}
	cases[4].ExpectedDocuments = []string{"synthetic-appeal"}
	cases[4].ExpectedCitations = []evaluation.CitationKey{bindings[0].Key, bindings[1].Key}
	cases[4].ExpectedTools = []evaluation.ToolExpectation{expectTool(tools.GetRuleDetailName, map[string]string{"mode": "current", "document_id": fmt.Sprint(bindings[0].IDs.DocumentID)}, "none")}
	cases[5].ExpectedTools = []evaluation.ToolExpectation{expectTool(tools.CheckAppealEligibilityName, map[string]string{"subject_type": "content", "subject_ref": "synthetic-video-001", "reason": "合成申诉原因"}, "none")}
	cases[6].ExpectedTools = []evaluation.ToolExpectation{expectTool(tools.CreateAppealTicketName, map[string]string{"subject_type": "content", "subject_ref": "synthetic-video-001", "reason": "合成申诉原因", "intake_mode": "manual_review"}, "none")}
	cases[4].ExpectedTools = append(cases[4].ExpectedTools, expectTool(tools.SearchRuleName, map[string]string{"query": "合成申诉规则"}, "none"))
	cases[6].ExpectedTools[0].ExpectedStatus = "rejected"
	if os.Getenv("EVALUATION_RULES_ONLY") == "1" {
		cases = cases[3:5]
	}
	scenarios := make([]evaluation.AgentScenario, len(cases))
	for i, c := range cases {
		scenarios[i] = evaluation.AgentScenario{CaseID: c.ID, Mode: "chat"}
	}
	t.Run("real_model_chat", func(t *testing.T) {
		_, report, records := run(t, "chat", "chat", cases, scenarios)
		// Model quality mismatches are measurements, not grounds to drop samples or edit expectations.
		liveSave(t, filepath.Join(evidence, "chat-quality.json"), report.Agent)
		for _, record := range records {
			var n int64
			liveMust(t, db.Model(&model.AppealTicket{}).Where("user_id = ?", record.UserID).Count(&n).Error)
			if n != 0 {
				t.Fatal("chat committed without trusted confirmation")
			}
		}
	})
	if os.Getenv("EVALUATION_RULES_ONLY") == "1" {
		return
	}
	t.Run("real_model_ticket_query", func(t *testing.T) {
		c := agentCase("query-ticket", "请查询本项目工单 $ticket_id 的当前状态，不要创建其他工单。", "ticket_status", false)
		c.ExpectedTools = []evaluation.ToolExpectation{expectTool(tools.QueryTicketStatusName, map[string]string{"ticket_id": "$ticket_id"}, "none")}
		_, report, records := run(t, "query", "ticket_query", []evaluation.Case{c}, []evaluation.AgentScenario{{CaseID: c.ID, Mode: "ticket_query", Input: &input}})
		liveSave(t, filepath.Join(evidence, "query-quality.json"), report.Agent)
		if records[0].Setup == nil || records[0].Setup.BusinessEffect != "committed" {
			t.Fatal("query setup missing")
		}
		var n int64
		liveMust(t, db.Model(&model.AppealTicket{}).Where("user_id = ?", records[0].UserID).Count(&n).Error)
		if n != 1 {
			t.Fatal("query added extra ticket")
		}
	})
}
