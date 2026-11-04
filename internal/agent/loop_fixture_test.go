package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"video-support-agent/internal/llm"
	"video-support-agent/internal/model"
	"video-support-agent/internal/retrieval"
	"video-support-agent/internal/service"
	"video-support-agent/internal/tools"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

type loopGenerate func(
	context.Context,
	[]*schema.Message,
) (*schema.Message, error)

type loopPlanner struct {
	r     *loopRig
	bound bool
}

func (p *loopPlanner) WithTools(
	infos []*schema.ToolInfo,
) (einomodel.ToolCallingChatModel, error) {
	p.r.binds++

	if p.bound {
		p.r.t.Fatal("binding mutated shared planner")
	}

	want := map[string]bool{
		tools.SearchRuleName:             true,
		tools.GetRuleDetailName:          true,
		tools.CheckAppealEligibilityName: true,
		tools.CreateAppealTicketName:     true,
		tools.QueryTicketStatusName:      true,
	}

	if len(infos) != len(want) {
		p.r.t.Fatal("tool count")
	}

	for _, info := range infos {
		if info == nil || !want[info.Name] || info.ParamsOneOf == nil {
			p.r.t.Fatal("tool schema")
		}
		delete(want, info.Name)
	}

	if p.r.bindErr != nil {
		return nil, p.r.bindErr
	}
	if p.r.bindNil {
		return nil, nil
	}

	return &loopPlanner{r: p.r, bound: true}, nil
}

func (p *loopPlanner) Generate(
	ctx context.Context,
	ms []*schema.Message,
	_ ...einomodel.Option,
) (*schema.Message, error) {
	if !p.bound {
		p.r.t.Fatal("unbound planner used")
	}

	p.r.plans++
	if !p.r.finalDeadline.IsZero() {
		p.r.t.Fatal("finalization started before planning ended")
	}
	loopDeadline(p.r.t, ctx, p.r.exec.models.Timeout)

	// 每条工具结果必须紧跟对应的、已清理附加信息的调用。
	for i, m := range ms {
		if m.Role != schema.Tool {
			continue
		}
		if i == 0 {
			p.r.t.Fatal("orphan result")
		}

		prev := ms[i-1]
		if prev.Role != schema.Assistant ||
			len(prev.ToolCalls) != 1 ||
			prev.ToolCalls[0].ID != m.ToolCallID ||
			prev.Content != "" ||
			prev.ReasoningContent != "" ||
			len(prev.Extra) != 0 ||
			len(prev.ToolCalls[0].Extra) != 0 {
			p.r.t.Fatal("tool-result association or sanitization")
		}
	}

	i := p.r.plans - 1
	if i >= len(p.r.steps) {
		p.r.t.Fatal("unexpected extra planner call")
	}
	return p.r.steps[i](ctx, ms)
}

func (p *loopPlanner) Stream(
	context.Context,
	[]*schema.Message,
	...einomodel.Option,
) (*schema.StreamReader[*schema.Message], error) {
	p.r.t.Fatal("unexpected streaming model call")
	return nil, nil
}

type loopAnswer struct {
	r *loopRig
}

func (a loopAnswer) Generate(
	ctx context.Context,
	ms []*schema.Message,
	_ ...einomodel.Option,
) (*schema.Message, error) {
	a.r.answers++
	loopDeadline(a.r.t, ctx, a.r.exec.models.Timeout)

	for _, m := range ms {
		if m.Role == schema.Tool || len(m.ToolCalls) != 0 {
			a.r.t.Fatal("planner tool history leaked to answer")
		}
	}

	if a.r.answer == nil {
		a.r.t.Fatal("unexpected answer call")
	}
	return a.r.answer(ctx, ms)
}

type loopRig struct {
	*confirmRig

	exec    *Executor
	planner *loopPlanner
	steps   []loopGenerate
	answer  loopGenerate

	binds     int
	plans     int
	answers   int
	retrieves int
	queries   int

	bindErr       error
	errorRetrieve error
	bindNil       bool

	result      retrieval.HybridResult
	calls       []model.ToolCall
	finishes    []service.AgentCallFinish
	corruptCall func(*model.ToolCall)
}

type loopRead struct {
	confirmReadDeps
	r *loopRig
}

func (d loopRead) Retrieve(
	ctx context.Context,
	q string,
	o retrieval.HybridOptions,
) (retrieval.HybridResult, error) {
	d.r.retrieves++
	loopDeadline(d.r.t, ctx, 8*time.Second)

	if strings.TrimSpace(q) == "" || o.TopK != 3 {
		d.r.t.Fatal("retrieval arguments")
	}
	return d.r.result, d.r.errorRetrieve
}

type loopAppeals struct {
	*service.AppealTicketService
	r *loopRig
}

func (a loopAppeals) Query(
	ctx context.Context,
	u, c, id uint64,
) (*service.AppealTicketView, error) {
	a.r.queries++
	loopDeadline(a.r.t, ctx, 8*time.Second)

	if u != 7 || c != 9 || id != 51 {
		a.r.t.Fatal("query identity")
	}

	return &service.AppealTicketView{
		TicketID:  51,
		Status:    model.AppealTicketPendingReview,
		CreatedAt: confirmAt,
		UpdatedAt: confirmAt,
	}, nil
}

func loopDeadline(
	t *testing.T,
	ctx context.Context,
	max time.Duration,
) {
	t.Helper()

	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) > max {
		t.Fatal("unbounded context")
	}
}

func newLoopRig(t *testing.T) *loopRig {
	t.Helper()

	r := &loopRig{
		confirmRig: newConfirmRig(t),
		result:     loopEvidence(),
	}
	r.planner = &loopPlanner{r: r}

	d := loopRead{
		confirmReadDeps: confirmReadDeps{r.confirmRig},
		r:               r,
	}

	var err error
	r.exec, err = NewExecutor(
		tools.RegistryDependencies{
			Conversations: d,
			Retriever:     d,
			Knowledge:     d,
			Appeals: loopAppeals{
				AppealTicketService: r.svc,
				r:                   r,
			},
		},
		r,
		r.svc,
		llm.AgentModels{
			Planner: r.planner,
			Answer:  loopAnswer{r},
			Timeout: time.Second,
		},
	)
	confirmOK(t, err)
	return r
}

// 明确虚构的内存资料，不写入知识库，也不访问来源URL。
func loopEvidence() retrieval.HybridResult {
	return retrieval.HybridResult{
		RetrievedAt: confirmAt,
		Outcome:     retrieval.HybridSuccess,
		Stats:       retrieval.HybridStats{FinalCount: 1},
		Items: []retrieval.HybridItem{
			{
				Rule: retrieval.HydratedChunk{
					Chunk: retrieval.RuleChunk{
						ChunkID:      11,
						DocumentID:   21,
						VersionID:    31,
						ArticleID:    41,
						CategoryID:   1,
						ArticleOrder: 1,
						Version:      "test-v1",
						Title:        "虚构测试规则",
						Content:      "虚构测试检索片段",
						SourceURL:    "https://www.bilibili.com/__loop_fixture__",
						EffectiveAt:  confirmAt.Add(-time.Hour),
					},
					ArticleContent: "虚构测试条款：满足条件A时可提交材料。",
				},
				ChunkIDs: []uint64{11},
			},
		},
	}
}

func (r *loopRig) StartCall(
	ctx context.Context,
	o service.AgentAuditOwner,
	id string,
	s service.AgentCallStart,
) (*model.ToolCall, error) {
	r.checkOwner(o)

	if id != confirmRunID || s.Binding != nil || s.ModelCallID == "" {
		r.t.Fatal("ordinary call binding")
	}
	confirmOK(r.t, service.ValidateAgentCallStart(o, s))

	if err := r.note(ctx, "start_call", 36*time.Second); err != nil {
		return nil, err
	}

	call := model.ToolCall{
		ID: fmt.Sprintf(
			"00000000-0000-4000-8000-%012d",
			len(r.calls)+10,
		),
		RunID:          id,
		Sequence:       uint32(len(r.calls) + 1),
		ToolName:       s.ToolName,
		ModelCallID:    &s.ModelCallID,
		ArgumentBytes:  s.ArgumentBytes,
		Status:         "started",
		ErrorCode:      "none",
		BusinessEffect: "none",
	}

	r.calls = append(r.calls, call)
	if r.corruptCall != nil {
		r.corruptCall(&call)
	}
	return &call, nil
}

func (r *loopRig) AuthorizeCreate(
	context.Context,
	service.AgentAuditOwner,
	string,
	string,
	service.AppealConfirmationBinding,
) error {
	r.t.Fatal("ordinary execution requested write permit")
	return nil
}

func (r *loopRig) FinishCall(
	ctx context.Context,
	o service.AgentAuditOwner,
	runID, callID string,
	f service.AgentCallFinish,
) error {
	r.checkOwner(o)

	if len(r.calls) == 0 ||
		runID != confirmRunID ||
		callID != r.calls[len(r.calls)-1].ID {
		r.t.Fatal("finish association")
	}
	confirmOK(r.t, service.ValidateAgentCallFinish(f))

	if f.BusinessEffect != "none" || f.Ticket != nil {
		r.t.Fatal("ordinary execution wrote ticket")
	}
	r.finishes = append(r.finishes, f)

	deadline, _ := ctx.Deadline()
	if time.Until(deadline) <= 3*time.Second {
		r.checkFinal(ctx)
	}
	return r.note(ctx, "finish_call", 36*time.Second)
}

func (r *loopRig) FinishRun(
	ctx context.Context,
	o service.AgentAuditOwner,
	id string,
	f service.AgentRunFinish,
) error {
	r.checkOwner(o)
	r.checkFinal(ctx)

	if id != confirmRunID ||
		f.ModelCalls != uint32(r.plans+r.answers) {
		r.t.Fatal("run model count")
	}
	confirmOK(r.t, service.ValidateAgentRunFinish(f))

	r.finishRun = &f
	return r.note(ctx, "finish_run", 3*time.Second)
}

func (r *loopRig) sink(
	ctx context.Context,
	e service.AgentEvent,
) error {
	if e.RunID != confirmRunID {
		r.t.Fatal("event run")
	}

	if e.Kind != "agent_start" {
		if len(r.calls) == 0 {
			r.t.Fatal("event before call")
		}
		c := r.calls[len(r.calls)-1]
		if e.CallID != c.ID ||
			e.Sequence != c.Sequence ||
			e.ToolName != c.ToolName {
			r.t.Fatal("event association")
		}
	}

	r.events = append(r.events, e)
	return r.note(ctx, e.Kind, 36*time.Second)
}

func loopRequest() service.AgentRequest {
	return service.AgentRequest{
		UserID:         7,
		ConversationID: 9,
		Question:       "虚构测试：提交材料需要什么条件？",
	}
}

func (r *loopRig) execute() (*service.AgentOutcome, error) {
	out, err := r.exec.Execute(
		context.Background(),
		loopRequest(),
		r.sink,
	)

	if r.txCalls != 0 ||
		out.BusinessEffect != "none" ||
		out.Ticket != nil ||
		out.MessageSaveStatus != "not_attempted" {
		r.t.Fatal("ordinary side effect")
	}
	return out, err
}

func loopMessage(raw string) *schema.Message {
	return &schema.Message{
		Role:         schema.Assistant,
		Content:      raw,
		ResponseMeta: &schema.ResponseMeta{FinishReason: "stop"},
	}
}

func loopTool(id, name, args string) *schema.Message {
	return &schema.Message{
		Role:             schema.Assistant,
		ResponseMeta:     &schema.ResponseMeta{FinishReason: "tool_calls"},
		ReasoningContent: "test-internal-reasoning",
		Extra:            map[string]any{"test": "private"},
		ToolCalls: []schema.ToolCall{
			{
				ID:    id,
				Type:  "function",
				Extra: map[string]any{"test": "private"},
				Function: schema.FunctionCall{
					Name:      name,
					Arguments: args,
				},
			},
		},
	}
}

func loopReply(m *schema.Message) loopGenerate {
	return func(
		context.Context,
		[]*schema.Message,
	) (*schema.Message, error) {
		return m, nil
	}
}

func loopSearch(n int) loopGenerate {
	return loopReply(loopTool(
		fmt.Sprintf("search-%d", n),
		tools.SearchRuleName,
		fmt.Sprintf("{\"query\":\"测试条件%d\"}", n),
	))
}

func loopHandle(t *testing.T, ms []*schema.Message) string {
	t.Helper()

	if len(ms) == 0 || ms[len(ms)-1].Role != schema.Tool {
		t.Fatal("missing search result")
	}

	var result tools.RuleSearchResult
	confirmOK(t, json.Unmarshal(
		[]byte(ms[len(ms)-1].Content),
		&result,
	))
	if len(result.EvidenceHandle) != 38 {
		t.Fatal("missing real evidence handle")
	}
	return result.EvidenceHandle
}

func loopAnswerPlan(t *testing.T) loopGenerate {
	return func(
		_ context.Context,
		ms []*schema.Message,
	) (*schema.Message, error) {
		return loopMessage(fmt.Sprintf(
			"{\"decision\":\"answer_rules\",\"evidence_handle\":%q}",
			loopHandle(t, ms),
		)), nil
	}
}

const loopValidAnswer = `{
	"decision":"answer",
	"statements":[{
		"text":"测试条件满足时可以提交材料。",
		"evidence":[{
			"ref_id":"R1",
			"quote":"满足条件A时可提交材料"
		}]
	}]
}`
