package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"video-support-agent/internal/llm"
	"video-support-agent/internal/model"
	"video-support-agent/internal/retrieval"
	"video-support-agent/internal/service"
	"video-support-agent/internal/tools"

	"github.com/cloudwego/eino/schema"
)

func TestAgentLoopTerminal(t *testing.T) {
	for _, name := range []string{
		"clarify", "handoff", "query", "proposal",
		"missing_facts", "manual_review", "nil_sink",
	} {
		t.Run(name, func(t *testing.T) {
			r := newLoopRig(t)
			want, reason := "clarify", "needs_more_info"
			r.steps = []loopGenerate{
				loopReply(loopMessage(`{"decision":"clarify"}`)),
			}

			switch name {
			case "handoff":
				want, reason = "handoff", "manual_review"
				r.steps[0] = loopReply(loopMessage(`{"decision":"handoff"}`))

			case "query":
				want, reason = "ticket_status", "none"
				r.steps[0] = loopReply(loopTool(
					"query", tools.QueryTicketStatusName,
					`{"ticket_id":"51"}`,
				))

			case "proposal":
				want, reason = "confirmation_required", "confirmation_required"
				raw, err := json.Marshal(r.request.Input)
				confirmOK(t, err)
				r.steps[0] = loopReply(loopTool(
					"create", tools.CreateAppealTicketName, string(raw),
				))

			case "missing_facts":
				r.steps[0] = loopReply(loopTool(
					"eligible", tools.CheckAppealEligibilityName,
					`{"subject_type":"account"}`,
				))

			case "manual_review":
				r.steps = []loopGenerate{
					loopReply(loopTool(
						"eligible", tools.CheckAppealEligibilityName,
						`{"subject_type":"account","subject_ref":"test-account","reason":"test-reason"}`,
					)),
					loopReply(loopMessage(`{"decision":"handoff"}`)),
				}
				want, reason = "handoff", "manual_review"
			}

			var out *service.AgentOutcome
			var err error
			if name == "nil_sink" {
				out, err = r.exec.Execute(
					context.Background(), loopRequest(), nil,
				)
			} else {
				out, err = r.execute()
			}
			confirmOK(t, err)

			if out.Decision != want ||
				out.ReasonCode != reason ||
				out.ExecutionStatus != "completed" ||
				out.AuditPending ||
				r.plans != len(r.steps) ||
				r.answers != 0 ||
				r.finishRun == nil ||
				r.binds != 1 ||
				r.planner.bound {
				t.Fatalf("out=%+v", out)
			}

			if name == "query" &&
				(out.TicketStatus == nil ||
					out.TicketStatus.TicketID != "51" ||
					r.queries != 1 ||
					!strings.Contains(out.Content, "pending_review")) {
				t.Fatal("query terminal result")
			}

			if name == "proposal" &&
				(out.Proposal == nil ||
					out.Proposal.Token == "" ||
					r.txCalls != 0 ||
					len(r.finishes) != 1 ||
					r.finishes[0].Status != "rejected" ||
					r.finishes[0].ErrorCode != "confirmation_required") {
				t.Fatal("proposal wrote or lost confirmation")
			}

			if name == "manual_review" {
				if len(r.finishes) != 1 ||
					r.finishes[0].Status != "succeeded" {
					t.Fatal("manual review did not continue")
				}
			}
		})
	}
}

func TestAgentLoopRAG(t *testing.T) {
	for _, name := range []string{
		"answer", "empty", "expired", "oversized_rules",
		"unknown_ref", "wrong_quote", "answer_tool_call",
		"insufficient", "clarify", "stale_handle", "foreign_handle",
	} {
		t.Run(name, func(t *testing.T) {
			r := newLoopRig(t)
			r.steps = []loopGenerate{
				loopSearch(1),
				loopAnswerPlan(t),
			}

			r.answer = func(
				_ context.Context,
				ms []*schema.Message,
			) (*schema.Message, error) {
				raw, err := json.Marshal(ms)
				confirmOK(t, err)

				if !strings.Contains(string(raw), "满足条件A时可提交材料") ||
					strings.Contains(string(raw), "test-internal-reasoning") {
					t.Fatal("RAG evidence context")
				}

				m := loopMessage(loopValidAnswer)
				switch name {
				case "unknown_ref":
					m.Content = strings.ReplaceAll(m.Content, "R1", "R9")
				case "wrong_quote":
					m.Content = strings.ReplaceAll(
						m.Content,
						"满足条件A时可提交材料",
						"不在原文中的测试引用",
					)
				case "answer_tool_call":
					m.ToolCalls = loopTool(
						"unexpected", tools.SearchRuleName, "{}",
					).ToolCalls
				case "insufficient":
					m.Content = `{"decision":"insufficient","statements":[]}`
				case "clarify":
					m.Content = `{"decision":"clarify","statements":[]}`
				}
				return m, nil
			}

			switch name {
			case "empty":
				r.result.Items = []retrieval.HybridItem{}
				r.result.Stats.FinalCount = 0
				r.result.Outcome = retrieval.HybridNoEvidence
				r.result.EmptyReason = "no_candidates"

			case "expired":
				expired := confirmAt
				r.result.Items[0].Rule.Chunk.ExpiresAt = &expired

			case "oversized_rules":
				r.result.Items[0].Rule.ArticleContent = strings.Repeat(
					"x", 25*1024,
				)

			case "stale_handle":
				var old string
				r.steps = []loopGenerate{
					loopSearch(1),
					func(
						ctx context.Context,
						ms []*schema.Message,
					) (*schema.Message, error) {
						old = loopHandle(t, ms)
						return loopSearch(2)(ctx, ms)
					},
					func(
						_ context.Context,
						ms []*schema.Message,
					) (*schema.Message, error) {
						if old == loopHandle(t, ms) {
							t.Fatal("handle reused")
						}
						return loopMessage(fmt.Sprintf(
							`{"decision":"answer_rules","evidence_handle":%q}`,
							old,
						)), nil
					},
				}

			case "foreign_handle":
				r.steps = []loopGenerate{
					loopReply(loopMessage(fmt.Sprintf(
						`{"decision":"answer_rules","evidence_handle":%q}`,
						strings.Repeat("x", 38),
					))),
				}
			}

			out, err := r.execute()
			if name == "stale_handle" || name == "foreign_handle" {
				confirmCode(t, err, "protocol_error")
				if r.answers != 0 {
					t.Fatal("unbound evidence answered")
				}
				return
			}
			confirmOK(t, err)

			want, reason, answers := "handoff", "no_evidence", 1
			if name == "answer" {
				want, reason = "answer", "none"
			}
			if name == "clarify" {
				want, reason = "clarify", "needs_more_info"
			}
			if name == "empty" ||
				name == "expired" ||
				name == "oversized_rules" {
				answers = 0
			}

			if out.Decision != want ||
				out.ReasonCode != reason ||
				r.answers != answers ||
				r.retrieves != 1 ||
				r.plans != 1+answers ||
				out.ExecutionStatus != "completed" {
				t.Fatalf(
					"out=%+v, calls=%d/%d retrieval=%d",
					out, r.plans, r.answers, r.retrieves,
				)
			}

			if name == "answer" {
				if len(out.Citations) != 1 ||
					out.Citations[0].RefID != "R1" ||
					out.Citations[0].ArticleID != "41" {
					t.Fatal("citations")
				}
			} else if len(out.Citations) != 0 {
				t.Fatal("fallback retained citations")
			}
		})
	}
}

func TestAgentLoopBudgets(t *testing.T) {
	for _, name := range []string{
		"repeat", "duplicate_id", "tool_limit", "answer_limit",
		"six_calls", "timeout", "cancelled", "request_size", "text_boundary",
	} {
		t.Run(name, func(t *testing.T) {
			if name == "text_boundary" {
				m := schema.ToolMessage(strings.Repeat("x", 32767), "a")
				if !messagesFit([]*schema.Message{m}) {
					t.Fatal("exact boundary rejected")
				}
				m.ToolName = "b"
				if messagesFit([]*schema.Message{m}) ||
					messagesFit([]*schema.Message{nil}) {
					t.Fatal("text overflow accepted")
				}

				m = schema.AssistantMessage(
					strings.Repeat("x", 32765),
					[]schema.ToolCall{
						{
							ID: "a",
							Function: schema.FunctionCall{
								Name: "b", Arguments: "c",
							},
						},
					},
				)
				if !messagesFit([]*schema.Message{m}) {
					t.Fatal("call boundary rejected")
				}
				m.ToolCalls[0].Function.Arguments += "d"
				if messagesFit([]*schema.Message{m}) {
					t.Fatal("call bytes not counted")
				}
				return
			}

			r := newLoopRig(t)
			r.steps = []loopGenerate{loopSearch(1)}

			want, code, plans, reads := "handoff", "", 2, 1
			reason := "repeated_call"
			ctx := context.Background()
			req := loopRequest()

			switch name {
			case "repeat":
				r.steps = append(r.steps, loopReply(loopTool(
					"new-id", tools.SearchRuleName,
					`{"top_k":3,"query":"  测试条件1  "}`,
				)))

			case "duplicate_id":
				r.steps = append(r.steps, loopReply(loopTool(
					"search-1", tools.SearchRuleName,
					`{"query":"不同测试条件"}`,
				)))
				want, code = "failed", "protocol_error"

			case "tool_limit", "answer_limit", "six_calls":
				n := 5
				if name == "six_calls" {
					n = 4
				}
				r.steps = nil
				for i := 1; i <= n; i++ {
					r.steps = append(r.steps, loopSearch(i))
				}
				if name == "tool_limit" {
					r.steps = append(r.steps, loopSearch(6))
				} else {
					r.steps = append(r.steps, loopAnswerPlan(t))
				}
				plans, reads, reason = 6, n, "budget_exhausted"

				if name == "six_calls" {
					plans = 5
					want, reason = "answer", "none"
					r.answer = loopReply(loopMessage(loopValidAnswer))
				}

			case "timeout":
				r.exec.models.Timeout = 5 * time.Millisecond
				r.steps = []loopGenerate{
					func(
						ctx context.Context,
						_ []*schema.Message,
					) (*schema.Message, error) {
						<-ctx.Done()
						return nil, ctx.Err()
					},
				}
				want, code, plans, reads = "failed", "timed_out", 1, 0

			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
				want, code, plans, reads = "failed", "cancelled", 0, 0

			case "request_size":
				req.Question = strings.Repeat("x", 32768)
				want, code, plans, reads = "failed", "budget_exhausted", 0, 0
			}

			out, err := r.exec.Execute(ctx, req, r.sink)
			confirmCode(t, err, code)

			if out.Decision != want ||
				r.plans != plans ||
				r.retrieves != reads ||
				len(out.Tools) != reads {
				t.Fatalf(
					"out=%+v calls=%d reads=%d",
					out, r.plans, r.retrieves,
				)
			}
			if code == "" && out.ReasonCode != reason {
				t.Fatal("fallback reason")
			}

			answers := 0
			if name == "six_calls" {
				answers = 1
			}
			if r.answers != answers ||
				r.plans+r.answers > 6 ||
				len(r.calls) > 5 {
				t.Fatal("budget")
			}
		})
	}
}

func TestAgentLoopFailures(t *testing.T) {
	for _, name := range []string{
		"bind", "nil_binding", "start_run", "nil_run", "run_owner",
		"start_call", "finish_call", "finish_run",
		"agent_start", "tool_start", "tool_end", "model", "retrieval",
	} {
		t.Run(name, func(t *testing.T) {
			r := newLoopRig(t)
			r.steps = []loopGenerate{
				loopSearch(1),
				loopReply(loopMessage(`{"decision":"handoff"}`)),
			}
			code, pending, reads := "audit_unavailable", false, 0

			switch name {
			case "bind":
				r.bindErr = errors.New("test-private-error")
				code = "configuration_error"

			case "nil_binding":
				r.bindNil = true
				code = "configuration_error"

			case "nil_run":
				r.badRun = "nil"
				code, pending = "invalid_result", true

			case "run_owner":
				r.badRun = "owner"
				code, pending = "invalid_result", true

			case "model":
				r.steps = []loopGenerate{
					func(
						context.Context,
						[]*schema.Message,
					) (*schema.Message, error) {
						return nil, errors.New("test-private-error")
					},
				}
				code = "model_failed"

			case "retrieval":
				r.errorRetrieve = errors.New("test-private-error")
				code, reads = "unavailable", 1

			default:
				r.failAt = name
				switch name {
				case "start_run", "start_call":
					pending = true
				case "finish_call", "finish_run":
					pending, reads = true, 1
				case "tool_end":
					reads, code = 1, "event_failed"
				case "agent_start", "tool_start":
					code = "event_failed"
				}
			}

			out, err := r.execute()
			confirmCode(t, err, code)

			if out.AuditPending != pending ||
				out.Decision != "failed" ||
				r.retrieves != reads ||
				out.ReasonCode != code ||
				strings.Contains(out.Content, "test-private-error") {
				t.Fatalf("out=%+v reads=%d", out, r.retrieves)
			}

			if (name == "start_call" || name == "finish_call") &&
				r.finishRun != nil {
				t.Fatal("closed uncertain audit")
			}
			if name == "tool_start" &&
				(len(r.finishes) != 1 ||
					r.finishes[0].ErrorCode != "event_failed") {
				t.Fatal("start event failure audit")
			}
		})
	}
}

func TestAgentLoopPreserveTerminal(t *testing.T) {
	for _, kind := range []string{"query", "proposal"} {
		for _, failure := range []string{
			"finish_call", "tool_end", "finish_run",
		} {
			t.Run(kind+"_"+failure, func(t *testing.T) {
				r := newLoopRig(t)
				r.failAt = failure

				m := loopTool(
					"terminal", tools.QueryTicketStatusName,
					`{"ticket_id":"51"}`,
				)
				decision := "ticket_status"

				if kind == "proposal" {
					raw, err := json.Marshal(r.request.Input)
					confirmOK(t, err)
					m = loopTool(
						"terminal", tools.CreateAppealTicketName, string(raw),
					)
					decision = "confirmation_required"
				}

				r.steps = []loopGenerate{loopReply(m)}
				out, err := r.execute()

				code := "audit_unavailable"
				if failure == "tool_end" {
					code = "event_failed"
				}
				confirmCode(t, err, code)

				if out.Decision != decision ||
					out.ExecutionStatus != "failed" ||
					out.AuditPending != (failure != "tool_end") ||
					r.plans != 1 ||
					r.answers != 0 ||
					r.txCalls != 0 {
					t.Fatalf("out=%+v", out)
				}

				if kind == "query" &&
					(out.TicketStatus == nil ||
						out.TicketStatus.TicketID != "51") {
					t.Fatal("lost query")
				}
				if kind == "proposal" &&
					(out.Proposal == nil || out.Proposal.Token == "") {
					t.Fatal("lost proposal")
				}
				if failure == "finish_call" && r.finishRun != nil {
					t.Fatal("run closed after uncertain call")
				}
			})
		}
	}
}

func TestAgentLoopProtocol(t *testing.T) {
	cases := []struct {
		name string
		edit func(*schema.Message)
	}{
		{"role", func(m *schema.Message) {
			m.Role = schema.User
		}},
		{"meta", func(m *schema.Message) {
			m.ResponseMeta = nil
		}},
		{"finish", func(m *schema.Message) {
			m.ResponseMeta.FinishReason = "length"
		}},
		{"tool_result_id", func(m *schema.Message) {
			m.ToolCallID = "unexpected"
		}},
		{"tool_result_name", func(m *schema.Message) {
			m.ToolName = "unexpected"
		}},
		{"content", func(m *schema.Message) {
			m.Content = "unexpected"
		}},
		{"content_size", func(m *schema.Message) {
			m.Content = strings.Repeat("x", 16385)
		}},
		{"content_utf8", func(m *schema.Message) {
			m.Content = string([]byte{255})
		}},
		{"reasoning_size", func(m *schema.Message) {
			m.ReasoningContent = strings.Repeat("x", 16385)
		}},
		{"multiple_calls", func(m *schema.Message) {
			m.ToolCalls = append(m.ToolCalls, m.ToolCalls[0])
		}},
		{"type", func(m *schema.Message) {
			m.ToolCalls[0].Type = "other"
		}},
		{"index", func(m *schema.Message) {
			n := 1
			m.ToolCalls[0].Index = &n
		}},
		{"name", func(m *schema.Message) {
			m.ToolCalls[0].Function.Name = "unknown"
		}},
		{"empty_id", func(m *schema.Message) {
			m.ToolCalls[0].ID = ""
		}},
		{"id_size", func(m *schema.Message) {
			m.ToolCalls[0].ID = strings.Repeat("x", 129)
		}},
		{"id_space", func(m *schema.Message) {
			m.ToolCalls[0].ID = "a b"
		}},
		{"empty_arguments", func(m *schema.Message) {
			m.ToolCalls[0].Function.Arguments = ""
		}},
		{"arguments_size", func(m *schema.Message) {
			m.ToolCalls[0].Function.Arguments = strings.Repeat("x", 16385)
		}},
		{"decision_finish", func(m *schema.Message) {
			*m = *loopMessage(`{"decision":"clarify"}`)
			m.ResponseMeta.FinishReason = "length"
		}},
		{"unknown_decision", func(m *schema.Message) {
			*m = *loopMessage(`{"decision":"unknown"}`)
		}},
		{"duplicate_field", func(m *schema.Message) {
			*m = *loopMessage(`{"decision":"clarify","decision":"handoff"}`)
		}},
		{"extra_field", func(m *schema.Message) {
			*m = *loopMessage(`{"decision":"clarify","extra":true}`)
		}},
		{"short_handle", func(m *schema.Message) {
			*m = *loopMessage(`{"decision":"answer_rules","evidence_handle":"short"}`)
		}},
		{"unexpected_handle", func(m *schema.Message) {
			*m = *loopMessage(`{"decision":"clarify","evidence_handle":"extra"}`)
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newLoopRig(t)
			m := loopTool(
				"test", tools.SearchRuleName, `{"query":"test"}`,
			)
			tc.edit(m)
			r.steps = []loopGenerate{loopReply(m)}

			out, err := r.execute()
			confirmCode(t, err, "protocol_error")

			if len(r.calls) != 0 ||
				r.retrieves != 0 ||
				out.Decision != "failed" ||
				r.finishRun == nil {
				t.Fatal("invalid protocol executed")
			}
		})
	}

	t.Run("nil_message", func(t *testing.T) {
		r := newLoopRig(t)
		r.steps = []loopGenerate{loopReply(nil)}
		_, err := r.execute()
		confirmCode(t, err, "protocol_error")

		if len(r.calls) != 0 {
			t.Fatal("nil response executed")
		}
	})

	t.Run("invalid_arguments", func(t *testing.T) {
		r := newLoopRig(t)
		r.steps = []loopGenerate{
			loopReply(loopTool(
				"test", tools.QueryTicketStatusName,
				`{"ticket_id":"0"}`,
			)),
		}
		_, err := r.execute()
		confirmCode(t, err, "invalid_argument")

		if len(r.calls) != 0 || r.queries != 0 {
			t.Fatal("invalid parameters executed")
		}
	})
}

func TestAgentLoopCallValidation(t *testing.T) {
	cases := []struct {
		name string
		edit func(*model.ToolCall)
	}{
		{"id", func(c *model.ToolCall) {
			c.ID = "invalid"
		}},
		{"run", func(c *model.ToolCall) {
			c.RunID = "other"
		}},
		{"sequence", func(c *model.ToolCall) {
			c.Sequence++
		}},
		{"tool", func(c *model.ToolCall) {
			c.ToolName = tools.QueryTicketStatusName
		}},
		{"status", func(c *model.ToolCall) {
			c.Status = "succeeded"
		}},
		{"error", func(c *model.ToolCall) {
			c.ErrorCode = "unavailable"
		}},
		{"effect", func(c *model.ToolCall) {
			c.BusinessEffect = "unknown"
		}},
		{"bytes", func(c *model.ToolCall) {
			c.ArgumentBytes++
		}},
		{"confirmed", func(c *model.ToolCall) {
			c.Confirmed = true
		}},
		{"authorized", func(c *model.ToolCall) {
			c.WriteAuthorized = true
		}},
		{"model_id", func(c *model.ToolCall) {
			id := "other"
			c.ModelCallID = &id
		}},
		{"nil_model_id", func(c *model.ToolCall) {
			c.ModelCallID = nil
		}},
		{"operation", func(c *model.ToolCall) {
			c.OperationKey = confirmRunID
		}},
		{"hash", func(c *model.ToolCall) {
			c.RequestHash = "unexpected"
		}},
		{"digest", func(c *model.ToolCall) {
			c.PolicyDigest = "unexpected"
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newLoopRig(t)
			r.corruptCall = tc.edit
			r.steps = []loopGenerate{loopSearch(1)}

			out, err := r.execute()
			confirmCode(t, err, "invalid_result")

			if !out.AuditPending ||
				r.retrieves != 0 ||
				r.finishRun != nil ||
				len(r.finishes) != 0 {
				t.Fatal("invalid audit association used")
			}
		})
	}
}

func TestAgentLoopInputAndConfiguration(t *testing.T) {
	for _, name := range []string{
		"blank", "utf8", "history_role", "history_blank",
		"history_limit", "history_order", "nil_planner", "nil_answer",
		"zero_timeout", "large_timeout", "nil_executor",
	} {
		t.Run(name, func(t *testing.T) {
			r := newLoopRig(t)
			req := loopRequest()

			switch name {
			case "nil_planner", "nil_answer", "zero_timeout", "large_timeout":
				models := r.exec.models
				switch name {
				case "nil_planner":
					models.Planner = nil
				case "nil_answer":
					models.Answer = nil
				case "zero_timeout":
					models.Timeout = 0
				case "large_timeout":
					models.Timeout = 31 * time.Second
				}

				_, err := NewExecutor(r.exec.deps, r, r.svc, models)
				if !errors.Is(err, service.ErrAgentAuditConfiguration) {
					t.Fatal(err)
				}
				return

			case "nil_executor":
				var e *Executor
				_, err := e.Execute(context.Background(), req, nil)
				confirmCode(t, err, "configuration_error")
				return

			case "blank":
				req.Question = " "

			case "utf8":
				req.Question = string([]byte{255})

			case "history_role":
				req.History = []llm.HistoryMessage{
					{Role: "tool", Content: "test"},
				}

			case "history_blank":
				req.History = []llm.HistoryMessage{
					{Role: "user", Content: " "},
				}

			case "history_limit":
				req.History = make([]llm.HistoryMessage, 101)

			case "history_order":
				req.History = []llm.HistoryMessage{
					{Role: "user", Content: "test-user"},
					{Role: "assistant", Content: "test-assistant"},
				}
				r.steps = []loopGenerate{
					func(
						_ context.Context,
						ms []*schema.Message,
					) (*schema.Message, error) {
						if len(ms) != 4 ||
							ms[0].Role != schema.System ||
							ms[1].Content != "test-user" ||
							ms[2].Content != "test-assistant" ||
							ms[3].Content != req.Question {
							t.Fatal("history order")
						}
						return loopMessage(`{"decision":"clarify"}`), nil
					},
				}
			}

			_, err := r.exec.Execute(context.Background(), req, nil)
			if name == "history_order" {
				confirmOK(t, err)
			} else {
				confirmCode(t, err, "invalid_argument")
				if r.binds != 0 || r.plans != 0 || len(r.trace) != 0 {
					t.Fatal("invalid input reached runtime")
				}
			}
		})
	}
}

func TestAgentLoopConfirmedDispatch(t *testing.T) {
	for _, name := range []string{
		"confirmed", "mixed_question", "mixed_history",
	} {
		t.Run(name, func(t *testing.T) {
			r := newLoopRig(t)

			// 确认分派使用第三块已验证的确认审计夹具。
			e := &Executor{
				ConfirmedExecutor: r.confirmRig.executor,
				models:            r.exec.models,
			}
			req := service.AgentRequest{
				UserID:         7,
				ConversationID: 9,
				Confirmed: &service.AgentConfirmation{
					Input:        r.request.Input,
					Confirmation: r.request.Confirmation,
				},
			}

			if name == "mixed_question" {
				req.Question = "test"
			}
			if name == "mixed_history" {
				req.History = []llm.HistoryMessage{
					{Role: "user", Content: "test"},
				}
			}

			out, err := e.Execute(context.Background(), req, nil)
			if name == "confirmed" {
				confirmOK(t, err)
				if out.BusinessEffect != "committed" ||
					out.Ticket == nil ||
					r.txCalls != 1 {
					t.Fatal("confirmation dispatch")
				}
			} else {
				confirmCode(t, err, "invalid_argument")
				if r.txCalls != 0 || len(r.trace) != 0 {
					t.Fatal("mixed input executed")
				}
			}

			if r.binds != 0 || r.plans != 0 || r.answers != 0 {
				t.Fatal("confirmation invoked model")
			}
		})
	}
}

func TestAgentLoopTransitions(t *testing.T) {
	for _, name := range []string{
		"eligible", "manual_review", "needs_more_info",
		"ineligible", "detail", "isolation", "rebind",
	} {
		t.Run(name, func(t *testing.T) {
			if name == "isolation" {
				first := newLoopRig(t)
				var handle string

				first.steps = []loopGenerate{
					loopSearch(1),
					func(
						_ context.Context,
						ms []*schema.Message,
					) (*schema.Message, error) {
						handle = loopHandle(t, ms)
						return loopMessage(`{"decision":"handoff"}`), nil
					},
				}
				_, err := first.execute()
				confirmOK(t, err)

				second := newLoopRig(t)
				second.steps = []loopGenerate{
					loopReply(loopMessage(fmt.Sprintf(
						`{"decision":"answer_rules","evidence_handle":%q}`,
						handle,
					))),
				}
				_, err = second.execute()
				confirmCode(t, err, "protocol_error")

				if second.answers != 0 || second.retrieves != 0 {
					t.Fatal("other run evidence used")
				}
				return
			}

			if name == "rebind" {
				r := newLoopRig(t)
				r.steps = []loopGenerate{
					loopReply(loopMessage(`{"decision":"clarify"}`)),
				}
				_, err := r.execute()
				confirmOK(t, err)

				r.resetAudit()
				r.plans = 0
				_, err = r.execute()
				confirmOK(t, err)

				if r.binds != 2 || r.planner.bound {
					t.Fatal("shared planner binding")
				}
				return
			}

			// 纯状态转换断言；真实服务路径在前面的用例中验证。
			s := &chatSession{out: &service.AgentOutcome{}}
			tool := tools.CheckAppealEligibilityName
			raw := fmt.Sprintf(`{"decision":%q}`, name)

			if name == "detail" {
				tool = tools.GetRuleDetailName
				raw = "{}"
			}

			stop, err := s.consume(tool, raw)
			confirmOK(t, err)

			expectedStop := name == "needs_more_info" || name == "ineligible"
			if stop != expectedStop {
				t.Fatal("wrong transition")
			}
			if name == "needs_more_info" && s.out.Decision != "clarify" {
				t.Fatal("missing facts")
			}
			if name == "ineligible" &&
				(s.out.Decision != "handoff" || s.out.ReasonCode != "ineligible") {
				t.Fatal("ineligible")
			}
		})
	}
}
