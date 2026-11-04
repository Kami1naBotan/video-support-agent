//go:build agentlive

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"video-support-agent/internal/config"
	"video-support-agent/internal/llm"
	"video-support-agent/internal/model"
	"video-support-agent/internal/retrieval"
	"video-support-agent/internal/service"
	"video-support-agent/internal/tools"

	"github.com/cloudwego/eino-ext/components/model/openai"
	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

type liveMessage struct {
	Role       string
	Content    string
	ToolCallID string
	Calls      []schema.ToolCall
	Finish     string
}

func liveProject(messages []*schema.Message) []liveMessage {
	result := make([]liveMessage, 0, len(messages))

	for _, message := range messages {
		if message == nil {
			continue
		}

		row := liveMessage{
			Role:       string(message.Role),
			Content:    message.Content,
			ToolCallID: message.ToolCallID,
			Calls:      []schema.ToolCall{},
		}
		if message.ResponseMeta != nil {
			row.Finish = message.ResponseMeta.FinishReason
		}

		for _, call := range message.ToolCalls {
			row.Calls = append(row.Calls, schema.ToolCall{
				ID:       call.ID,
				Type:     call.Type,
				Function: call.Function,
			})
		}

		result = append(result, row)
	}
	return result
}

type liveExchange struct {
	Kind          string
	Input, Output []liveMessage
	ErrorKind     string
	ElapsedMS     int64
}

type liveTrace struct {
	r           *loopRig
	Exchanges   []liveExchange
	DetailReads int
	Bindings    [][]string
}

func liveError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timed_out"
	}

	var api *openai.APIError
	if errors.As(err, &api) && api != nil {
		switch {
		case api.HTTPStatusCode == 401:
			return "authentication"
		case api.HTTPStatusCode == 403:
			return "permission"
		case api.HTTPStatusCode == 429:
			return "rate_or_quota"
		case api.HTTPStatusCode >= 500:
			return "upstream"
		default:
			return "request_rejected"
		}
	}

	var network net.Error
	if errors.As(err, &network) {
		return "network"
	}
	return "model_error"
}

func (l *liveTrace) generate(
	ctx context.Context,
	generator llm.Generator,
	kind string,
	messages []*schema.Message,
	options ...einomodel.Option,
) (*schema.Message, error) {
	if kind == "planner" {
		l.r.plans++
	} else {
		l.r.answers++
	}

	row := liveExchange{
		Kind:  kind,
		Input: liveProject(messages),
	}

	started := time.Now()
	output, err := generator.Generate(ctx, messages, options...)
	row.ElapsedMS = time.Since(started).Milliseconds()
	row.Output = liveProject([]*schema.Message{output})
	row.ErrorKind = liveError(err)
	l.Exchanges = append(l.Exchanges, row)

	// 原样交回真实模型结果，不修正协议、不替换答案、不重试。
	return output, err
}

type livePlanner struct {
	base  einomodel.ToolCallingChatModel
	trace *liveTrace
	bound bool
}

func (p *livePlanner) WithTools(
	infos []*schema.ToolInfo,
) (einomodel.ToolCallingChatModel, error) {
	names := make([]string, 0, len(infos))
	for _, info := range infos {
		if info == nil || info.ParamsOneOf == nil {
			return nil, errors.New("invalid tool schema")
		}
		names = append(names, info.Name)
	}
	p.trace.Bindings = append(p.trace.Bindings, names)

	bound, err := p.base.WithTools(infos)
	if err != nil {
		return nil, err
	}

	return &livePlanner{
		base:  bound,
		trace: p.trace,
		bound: true,
	}, nil
}

func (p *livePlanner) Generate(
	ctx context.Context,
	messages []*schema.Message,
	options ...einomodel.Option,
) (*schema.Message, error) {
	if !p.bound {
		return nil, errors.New("unbound planner")
	}
	return p.trace.generate(ctx, p.base, "planner", messages, options...)
}

func (p *livePlanner) Stream(
	context.Context,
	[]*schema.Message,
	...einomodel.Option,
) (*schema.StreamReader[*schema.Message], error) {
	return nil, errors.New("stream not used by agent executor")
}

type liveAnswer struct {
	base  llm.Generator
	trace *liveTrace
}

func (a liveAnswer) Generate(
	ctx context.Context,
	messages []*schema.Message,
	options ...einomodel.Option,
) (*schema.Message, error) {
	return a.trace.generate(ctx, a.base, "answer", messages, options...)
}

type liveRead struct {
	confirmReadDeps
	trace *liveTrace
}

func (d liveRead) Retrieve(
	ctx context.Context,
	question string,
	options retrieval.HybridOptions,
) (retrieval.HybridResult, error) {
	d.trace.r.retrieves++

	if strings.TrimSpace(question) == "" ||
		options.TopK < 1 || options.TopK > 3 {
		return retrieval.HybridResult{}, service.ErrInvalidArgument
	}

	return loopEvidence(), ctx.Err()
}

func (d liveRead) GetCurrentByDocumentID(
	ctx context.Context,
	id uint64,
) (*service.KnowledgeCurrentDetail, error) {
	d.trace.DetailReads++
	if id != 21 {
		return nil, service.ErrKnowledgeDocumentNotFound
	}

	at := confirmAt.Add(-time.Hour)
	return &service.KnowledgeCurrentDetail{
		Version: &model.RuleVersion{
			ID:          31,
			DocumentID:  21,
			CategoryID:  1,
			Version:     "test-v1",
			Title:       "虚构测试规则",
			Content:     "虚构测试条款：满足条件A时可提交材料。",
			SourceURL:   "https://www.bilibili.com/__loop_fixture__",
			Status:      model.RuleVersionStatusPublished,
			EffectiveAt: &at,
			PublishedAt: &at,
		},
		Articles: []model.RuleArticle{
			{
				ID:           41,
				VersionID:    31,
				ArticleOrder: 1,
				Content:      "虚构测试条款：满足条件A时可提交材料。",
			},
		},
	}, ctx.Err()
}

func TestAgentLiveModel(t *testing.T) {
	outputDir := os.Getenv("AGENT_LIVE_OUTPUT")
	if os.Getenv("AGENT_LIVE_ACCEPT") != "1" ||
		!filepath.IsAbs(outputDir) {
		t.Fatal("use the dedicated live acceptance script")
	}

	traceLiveHTTP(t, outputDir)
	cfg := config.LLMConfig{
		BaseURL: os.Getenv("AGENT_LIVE_BASE_URL"),
		Model:   os.Getenv("AGENT_LIVE_MODEL"),
		APIKey:  os.Getenv("AGENT_LIVE_API_KEY"),
		Timeout: os.Getenv("AGENT_LIVE_TIMEOUT"),
	}

	cases := []struct {
		name     string
		question string
		decision string
		required []string
	}{
		{
			name:     "query",
			question: "请查询我在本项目中的工单51当前状态。",
			decision: "ticket_status",
			required: []string{tools.QueryTicketStatusName},
		},
		{
			name: "proposal",
			question: "我要创建本项目人工复核工单。" +
				"事项是账号问题，账号标识为test-account，" +
				"申请说明为测试账号资料需要人工复核。" +
				"我选择人工复核受理方式，请先准备确认信息，" +
				"我还没有确认提交。",
			decision: "confirmation_required",
			required: []string{tools.CreateAppealTicketName},
		},
		{
			name: "eligibility",
			question: "请检查我的申诉资格：事项为账号问题，" +
				"账号标识test-account，申请说明为测试账号资料需要人工复核。" +
				"本次只检查资格，不创建工单；" +
				"如果没有可靠政策依据，请告知需要人工核对。",
			decision: "handoff",
			required: []string{tools.CheckAppealEligibilityName},
		},
		{
			name: "rules",
			question: "请搜索虚构测试规则：满足条件A时是否可以提交材料？" +
				"请依据检索到的原文回答并提供引用。",
			decision: "answer",
			required: []string{tools.SearchRuleName},
		},
		{
			name: "detail",
			question: "请读取规则文档21的当前详情，并搜索相关规则，" +
				"依据原文回答满足条件A时是否可以提交材料，提供引用。",
			decision: "answer",
			required: []string{
				tools.GetRuleDetailName,
				tools.SearchRuleName,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "eligibility" {
				if question := os.Getenv("AGENT_LIVE_ELIGIBILITY_QUESTION"); question != "" {
					tc.question = question
				}
			}

			r := newLoopRig(t)
			trace := &liveTrace{
				r:         r,
				Exchanges: []liveExchange{},
				Bindings:  [][]string{},
			}

			var out *service.AgentOutcome
			var executeErr error

			// 断言失败也保存已收到的模型事实。
			// 不保存确认Token、完整提案或原始API错误。
			t.Cleanup(func() {
				result := map[string]any{
					"Scenario":         tc.name,
					"Question":         tc.question,
					"Model":            cfg.Model,
					"Exchanges":        trace.Exchanges,
					"Bindings":         trace.Bindings,
					"PlannerCalls":     r.plans,
					"AnswerCalls":      r.answers,
					"RetrievalCalls":   r.retrieves,
					"DetailReads":      trace.DetailReads,
					"QueryReads":       r.queries,
					"TransactionCalls": r.txCalls,
					"Events":           r.events,
					"Passed":           !t.Failed(),
					"SemanticReview":   "pending",
				}

				if out != nil {
					result["Decision"] = out.Decision
					result["ReasonCode"] = out.ReasonCode
					result["ExecutionStatus"] = out.ExecutionStatus
					result["BusinessEffect"] = out.BusinessEffect
					result["AuditPending"] = out.AuditPending
					result["Content"] = out.Content
					result["Citations"] = out.Citations
					result["Tools"] = out.Tools
					result["ProposalPresent"] = out.Proposal != nil
					result["TicketStatus"] = out.TicketStatus
				}
				if executeErr != nil {
					result["ExecutionErrorCode"] = errorCode(executeErr)
				}

				data, err := json.MarshalIndent(result, "", "  ")
				if err != nil {
					t.Error("encode live evidence")
					return
				}

				path := filepath.Join(outputDir, tc.name+".json")
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Error("save live evidence")
				}
			})

			setup, cancel := context.WithTimeout(
				context.Background(),
				30*time.Second,
			)
			models, err := llm.NewAgentModels(setup, cfg)
			cancel()
			if err != nil {
				t.Fatal("model configuration failed; raw error omitted")
			}

			models.Planner = &livePlanner{
				base:  models.Planner,
				trace: trace,
			}
			models.Answer = liveAnswer{
				base:  models.Answer,
				trace: trace,
			}

			reads := liveRead{
				confirmReadDeps: confirmReadDeps{r.confirmRig},
				trace:           trace,
			}
			r.exec, err = NewExecutor(
				tools.RegistryDependencies{
					Conversations: reads,
					Retriever:     reads,
					Knowledge:     reads,
					Appeals: loopAppeals{
						AppealTicketService: r.svc,
						r:                   r,
					},
				},
				r,
				r.svc,
				models,
			)
			confirmOK(t, err)

			req := loopRequest()
			req.Question = tc.question
			out, executeErr = r.exec.Execute(
				context.Background(),
				req,
				r.sink,
			)
			if executeErr != nil {
				t.Fatalf(
					"execution code=%s; inspect retained evidence",
					errorCode(executeErr),
				)
			}

			if out.ExecutionStatus != "completed" ||
				out.Decision != tc.decision ||
				out.AuditPending ||
				out.BusinessEffect != "none" ||
				out.Ticket != nil ||
				r.txCalls != 0 ||
				r.finishRun == nil {
				t.Fatalf(
					"unexpected decision/status: %s/%s",
					out.Decision,
					out.ExecutionStatus,
				)
			}

			if r.plans < 1 ||
				r.plans+r.answers > 6 ||
				len(out.Tools) > 5 ||
				len(trace.Bindings) != 1 ||
				len(trace.Bindings[0]) != 5 {
				t.Fatal("model/tool binding or budgets")
			}

			seen := map[string]bool{}
			for _, call := range out.Tools {
				seen[call.ToolName] = true
			}
			for _, name := range tc.required {
				if !seen[name] {
					t.Errorf("required tool not executed: %s", name)
				}
			}

			switch tc.name {
			case "query":
				if r.queries != 1 ||
					out.TicketStatus == nil ||
					out.TicketStatus.TicketID != "51" ||
					r.answers != 0 {
					t.Fatal("query outcome")
				}

			case "proposal":
				if out.Proposal == nil ||
					out.Proposal.Input.IntakeMode != model.AppealIntakeManualReview ||
					r.answers != 0 {
					t.Fatal("proposal outcome")
				}

			case "eligibility":
				if out.Proposal != nil ||
					out.ReasonCode != "manual_review" ||
					seen[tools.CreateAppealTicketName] {
					t.Fatal("eligibility scope")
				}

			case "rules", "detail":
				if r.answers != 1 ||
					r.retrieves < 1 ||
					len(out.Citations) != 1 ||
					out.Citations[0].ArticleID != "41" {
					t.Fatal("grounded answer")
				}
				if tc.name == "detail" && trace.DetailReads < 1 {
					t.Fatal("detail not read")
				}
			}

			// 检查真实适配器收到的回填消息。
			// 查询/提案已经终止，不应再把结果交给模型。
			paired := 0
			for _, exchange := range trace.Exchanges {
				for i, message := range exchange.Input {
					if message.Role != "tool" {
						continue
					}
					if exchange.Kind != "planner" || i == 0 {
						t.Fatal("tool result in wrong model")
					}

					previous := exchange.Input[i-1]
					if previous.Role != "assistant" ||
						len(previous.Calls) != 1 ||
						previous.Calls[0].ID != message.ToolCallID {
						t.Fatal("tool result ID mismatch")
					}

					name := previous.Calls[0].Function.Name
					if name == tools.QueryTicketStatusName ||
						name == tools.CreateAppealTicketName {
						t.Fatal("terminal result returned to model")
					}
					paired++
				}
			}

			if (tc.name == "rules" ||
				tc.name == "detail" ||
				tc.name == "eligibility") && paired == 0 {
				t.Fatal("no real tool-result roundtrip")
			}
		})
	}
}
