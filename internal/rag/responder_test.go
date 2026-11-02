package rag_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"video-support-agent/internal/llm"
	"video-support-agent/internal/model"
	"video-support-agent/internal/rag"
	"video-support-agent/internal/retrieval"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"go.uber.org/zap"
)

type responderRetrieverFunc func(
	context.Context,
	string,
	retrieval.HybridOptions,
) (retrieval.HybridResult, error)

func (f responderRetrieverFunc) Retrieve(
	ctx context.Context,
	query string,
	options retrieval.HybridOptions,
) (retrieval.HybridResult, error) {
	return f(ctx, query, options)
}

type responderGeneratorFunc func(
	context.Context,
	[]*schema.Message,
) (*schema.Message, error)

func (f responderGeneratorFunc) Generate(
	ctx context.Context,
	messages []*schema.Message,
	_ ...einomodel.Option,
) (*schema.Message, error) {
	return f(ctx, messages)
}

type responderHarness struct {
	result      retrieval.HybridResult
	retrieveErr error
	raw         *schema.Message
	generateErr error

	onRetrieve responderRetrieverFunc
	onGenerate responderGeneratorFunc

	retrieveCalls int
	generateCalls int

	workCtx       context.Context
	generationCtx context.Context
	query         string
	options       retrieval.HybridOptions
	messages      []*schema.Message

	enteredRetrieve time.Time
	enteredGenerate time.Time
}

func newResponderHarness(t *testing.T) *responderHarness {
	t.Helper()
	return &responderHarness{
		result: fixtureResult(fixtureItem(1)),
		raw: outputMessage(
			outputJSON(t, "answer", normalAnswerRow()),
		),
	}
}

func (h *responderHarness) responder(
	t *testing.T,
	timeout time.Duration,
	logger *zap.Logger,
) *rag.Responder {
	t.Helper()

	retriever := responderRetrieverFunc(func(
		ctx context.Context,
		query string,
		options retrieval.HybridOptions,
	) (retrieval.HybridResult, error) {
		h.retrieveCalls++
		h.workCtx = ctx
		h.query = query
		h.options = options
		h.enteredRetrieve = time.Now()

		if h.onRetrieve != nil {
			return h.onRetrieve(ctx, query, options)
		}
		return h.result, h.retrieveErr
	})

	generator := responderGeneratorFunc(func(
		ctx context.Context,
		messages []*schema.Message,
	) (*schema.Message, error) {
		h.generateCalls++
		h.generationCtx = ctx
		h.messages = messages
		h.enteredGenerate = time.Now()

		if h.onGenerate != nil {
			return h.onGenerate(ctx, messages)
		}
		return h.raw, h.generateErr
	})

	responder, err := rag.NewResponder(
		retriever, generator, timeout, logger,
	)
	if err != nil {
		t.Fatal(err)
	}
	return responder
}

func (h *responderHarness) requireCalls(
	t *testing.T,
	retrieve, generate int,
) {
	t.Helper()
	if h.retrieveCalls != retrieve || h.generateCalls != generate {
		t.Fatalf(
			"calls: retrieve=%d generate=%d; want %d/%d",
			h.retrieveCalls, h.generateCalls, retrieve, generate,
		)
	}
}

func requireResponderAnswer(
	t *testing.T,
	reply model.GroundedReply,
	err error,
) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	if reply.Decision != model.ReplyAnswer ||
		len(reply.Citations) != 1 ||
		reply.Citations[0].RefID != "R1" ||
		reply.Citations[0].ArticleID != "401" ||
		!strings.Contains(reply.Content, "测试陈述") {
		t.Fatalf("unexpected answer: %+v", reply)
	}
}

func requireResponderFixed(
	t *testing.T,
	reply model.GroundedReply,
	err error,
	reason rag.FixedReason,
) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}

	// 固定文案本身已在板块1测试；这里验证编排选中了正确分支。
	wantText, err := rag.FixedText(reason)
	if err != nil {
		t.Fatal(err)
	}
	wantDecision := model.ReplyInsufficient
	if reason == rag.FixedQuestionTooLong ||
		reason == rag.FixedContextTooLarge {
		wantDecision = model.ReplyClarify
	}

	if reply.Decision != wantDecision ||
		reply.Content != wantText ||
		reply.Citations == nil ||
		len(reply.Citations) != 0 {
		t.Fatalf("unexpected fixed reply: %+v", reply)
	}
}

func requireResponderContextDone(
	t *testing.T,
	ctx context.Context,
	want error,
) {
	t.Helper()
	if ctx == nil {
		t.Fatal("dependency context was not captured")
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("dependency context was not released")
	}
	if !errors.Is(ctx.Err(), want) {
		t.Fatalf("context error=%v, want %v", ctx.Err(), want)
	}
}

func TestRAGResponderConfiguration(t *testing.T) {
	retriever := responderRetrieverFunc(func(
		context.Context, string, retrieval.HybridOptions,
	) (retrieval.HybridResult, error) {
		return fixtureResult(), nil
	})
	generator := responderGeneratorFunc(func(
		context.Context, []*schema.Message,
	) (*schema.Message, error) {
		return nil, nil
	})

	cases := []struct {
		name      string
		retriever retrieval.HybridRetriever
		generator llm.Generator
		timeout   time.Duration
	}{
		{"nil_retriever", nil, generator, time.Second},
		{"nil_generator", retriever, nil, time.Second},
		{"zero_timeout", retriever, generator, 0},
		{"negative_timeout", retriever, generator, -time.Second},
		{"over_maximum", retriever, generator, 30*time.Second + 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := rag.NewResponder(
				tc.retriever, tc.generator, tc.timeout, nil,
			)
			if got != nil || !errors.Is(err, rag.ErrResponderConfiguration) {
				t.Fatalf("responder=%v error=%v", got, err)
			}
		})
	}

	t.Run("maximum_and_nil_logger", func(t *testing.T) {
		h := newResponderHarness(t)
		r := h.responder(t, 30*time.Second, nil)
		reply, err := r.ReplyGrounded(context.Background(), nil, "测试问题")
		requireResponderAnswer(t, reply, err)
		h.requireCalls(t, 1, 1)
	})

	t.Run("nil_receiver", func(t *testing.T) {
		var r *rag.Responder
		reply, err := r.ReplyGrounded(context.Background(), nil, "测试问题")
		requireAnswerError(t, reply, err, rag.ErrResponderConfiguration)

		text, err := r.Reply(context.Background(), nil, "测试问题")
		if text != "" || !errors.Is(err, rag.ErrResponderConfiguration) {
			t.Fatalf("legacy reply=%q error=%v", text, err)
		}
	})

	t.Run("zero_receiver", func(t *testing.T) {
		var r rag.Responder
		reply, err := r.ReplyGrounded(context.Background(), nil, "测试问题")
		requireAnswerError(t, reply, err, rag.ErrResponderConfiguration)
	})
}

func TestRAGResponderInputShortCircuit(t *testing.T) {
	cases := []struct {
		name     string
		question string
		mode     string
		wantErr  error
		fixed    rag.FixedReason
	}{
		{"blank", " \n\t", "", llm.ErrInvalidInput, rag.FixedNone},
		{"invalid_utf8", string([]byte{0xff}), "", llm.ErrInvalidInput, rag.FixedNone},
		{"over_chat_limit", strings.Repeat("问", 10001), "", llm.ErrInvalidInput, rag.FixedNone},
		{"over_query_limit", strings.Repeat("问", 201), "", nil, rag.FixedQuestionTooLong},
		{"chat_limit", strings.Repeat("问", 10000), "", nil, rag.FixedQuestionTooLong},
		{"parent_canceled", "测试问题", "canceled", context.Canceled, rag.FixedNone},
		{"reserve_exhausted", "测试问题", "reserve", context.DeadlineExceeded, rag.FixedNone},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			switch tc.mode {
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "reserve":
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 3*time.Second)
				defer cancel()
			}

			h := newResponderHarness(t)
			r := h.responder(t, time.Second, nil)
			reply, err := r.ReplyGrounded(ctx, nil, tc.question)

			if tc.wantErr != nil {
				requireAnswerError(t, reply, err, tc.wantErr)
			} else {
				requireResponderFixed(t, reply, err, tc.fixed)
			}
			h.requireCalls(t, 0, 0)

			if tc.mode == "reserve" && ctx.Err() != nil {
				t.Fatalf("parent should still be live: %v", ctx.Err())
			}
		})
	}
}

func TestRAGResponderRetrievalBranches(t *testing.T) {
	t.Run("usable_success_and_degraded", func(t *testing.T) {
		for _, degraded := range []bool{false, true} {
			name := "success"
			if degraded {
				name = "degraded"
			}
			t.Run(name, func(t *testing.T) {
				h := newResponderHarness(t)
				if degraded {
					h.result.Outcome = retrieval.HybridDegraded
					h.result.Degraded = true
				}
				r := h.responder(t, time.Second, nil)
				reply, err := r.ReplyGrounded(context.Background(), nil, "测试问题")
				requireResponderAnswer(t, reply, err)
				h.requireCalls(t, 1, 1)
			})
		}
	})

	fixedCases := []struct {
		name   string
		change func(*retrieval.HybridResult)
		reason rag.FixedReason
	}{
		{"no_candidates", func(r *retrieval.HybridResult) {
			*r = fixtureResult()
		}, rag.FixedNoEvidence},
		{"current_filter", func(r *retrieval.HybridResult) {
			*r = fixtureResult()
			r.EmptyReason = "current_filter"
		}, rag.FixedNoEvidence},
		{"degraded_empty", func(r *retrieval.HybridResult) {
			*r = fixtureResult()
			r.Outcome = retrieval.HybridDegradedEmpty
			r.Degraded = true
		}, rag.FixedDegradedEmpty},
		{"conflicts", func(r *retrieval.HybridResult) {
			*r = fixtureResult()
			r.Outcome = retrieval.HybridNoUsableEvidence
			r.EmptyReason = "conflicts"
		}, rag.FixedUnusableEvidence},
		{"unusable_source", func(r *retrieval.HybridResult) {
			r.Items[0].Rule.Chunk.SourceURL = ""
		}, rag.FixedUnusableEvidence},
		{"context_too_large", func(r *retrieval.HybridResult) {
			r.Items[0].Rule.ArticleContent = strings.Repeat("X", 25<<10)
		}, rag.FixedContextTooLarge},
	}
	for _, tc := range fixedCases {
		t.Run(tc.name, func(t *testing.T) {
			h := newResponderHarness(t)
			tc.change(&h.result)
			r := h.responder(t, time.Second, nil)
			reply, err := r.ReplyGrounded(context.Background(), nil, "测试问题")
			requireResponderFixed(t, reply, err, tc.reason)
			h.requireCalls(t, 1, 0)
		})
	}

	for _, upstream := range []error{
		errors.New("PRIVATE_RETRIEVAL_ERROR"),
		retrieval.ErrHybridUnavailable,
		retrieval.ErrHydrationFailed,
		context.DeadlineExceeded,
		context.Canceled,
	} {
		t.Run("error_"+upstream.Error(), func(t *testing.T) {
			h := newResponderHarness(t)
			h.retrieveErr = upstream
			r := h.responder(t, time.Second, nil)
			reply, err := r.ReplyGrounded(context.Background(), nil, "测试问题")
			requireAnswerError(t, reply, err, rag.ErrRetrievalFailed)
			if err != rag.ErrRetrievalFailed {
				t.Fatalf("upstream details escaped: %v", err)
			}
			h.requireCalls(t, 1, 0)
		})
	}

	invalidCases := []struct {
		name   string
		change func(*retrieval.HybridResult)
	}{
		{"wrong_count", func(r *retrieval.HybridResult) {
			r.Stats.FinalCount++
		}},
		{"invalid_identity", func(r *retrieval.HybridResult) {
			r.Items[0].Rule.Chunk.DocumentID = 0
		}},
		{"read_budget", func(r *retrieval.HybridResult) {
			r.Items[0].Rule.ArticleContent = strings.Repeat(
				"X", retrieval.MaxHydrationRowBytes+1,
			)
		}},
	}
	for _, tc := range invalidCases {
		t.Run(tc.name, func(t *testing.T) {
			h := newResponderHarness(t)
			tc.change(&h.result)
			r := h.responder(t, time.Second, nil)
			reply, err := r.ReplyGrounded(context.Background(), nil, "测试问题")
			requireAnswerError(t, reply, err, rag.ErrRetrievalFailed)
			h.requireCalls(t, 1, 0)
		})
	}

	t.Run("invalid_history", func(t *testing.T) {
		h := newResponderHarness(t)
		r := h.responder(t, time.Second, nil)
		reply, err := r.ReplyGrounded(
			context.Background(),
			[]llm.HistoryMessage{{Role: "system", Content: "invalid history role"}},
			"测试问题",
		)
		requireAnswerError(t, reply, err, rag.ErrInvalidHistory)
		h.requireCalls(t, 1, 0)
	})
}

func TestRAGResponderPromptAndResult(t *testing.T) {
	type valueKey struct{}
	parent := context.WithValue(context.Background(), valueKey{}, "request-value")

	first, second := fixtureItem(1), fixtureItem(2)
	first.Rule.Chunk.Title = "第一份虚构规则"
	second.Rule.Chunk.Title = "第二份虚构规则"
	second.Rule.Chunk.ArticleNumber = "TEST-2"

	h := newResponderHarness(t)
	h.result = fixtureResult(first, second)
	row := normalAnswerRow()
	row.Evidence[0].RefID = "R2"
	h.raw = outputMessage(outputJSON(t, "answer", row))
	history := []llm.HistoryMessage{
		{Role: "user", Content: "旧问题"},
		{Role: "assistant", Content: "旧回答"},
	}
	r := h.responder(t, 2*time.Second, nil)
	reply, err := r.ReplyGrounded(parent, history, "  本次测试问题 \n")
	if err != nil {
		t.Fatal(err)
	}
	h.requireCalls(t, 1, 1)

	if h.query != "本次测试问题" ||
		h.options != (retrieval.HybridOptions{TopK: 5}) {
		t.Fatalf("query=%q options=%+v", h.query, h.options)
	}
	if len(h.messages) != 4 {
		t.Fatalf("message count=%d", len(h.messages))
	}
	wantRoles := []schema.RoleType{
		schema.System, schema.User, schema.Assistant, schema.User,
	}
	for i, role := range wantRoles {
		if h.messages[i] == nil || h.messages[i].Role != role {
			t.Fatalf("unexpected message role at %d", i)
		}
	}
	if h.messages[0].Content == "" ||
		strings.Contains(h.messages[0].Content, "本次测试问题") ||
		h.messages[1].Content != "旧问题" ||
		h.messages[2].Content != "旧回答" {
		t.Fatal("system/history/current separation was not preserved")
	}

	var current struct {
		Question string `json:"question"`
		Rules    []struct {
			RefID          string `json:"ref_id"`
			DocumentID     string `json:"document_id"`
			VersionID      string `json:"version_id"`
			ArticleID      string `json:"article_id"`
			ArticleNumber  string `json:"article_number"`
			ArticleContent string `json:"article_content"`
		} `json:"rules"`
	}
	if err := json.Unmarshal([]byte(h.messages[3].Content), &current); err != nil {
		t.Fatal(err)
	}
	if current.Question != "本次测试问题" || len(current.Rules) != 2 {
		t.Fatalf("unexpected current payload: %+v", current)
	}
	if current.Rules[0].RefID != "R1" ||
		current.Rules[0].DocumentID != "201" ||
		current.Rules[0].VersionID != "301" ||
		current.Rules[0].ArticleID != "401" ||
		current.Rules[0].ArticleContent != first.Rule.ArticleContent ||
		current.Rules[1].RefID != "R2" ||
		current.Rules[1].ArticleNumber != "TEST-2" ||
		current.Rules[1].ArticleContent != second.Rule.ArticleContent {
		t.Fatalf("full current rules were not passed through: %+v", current.Rules)
	}
	if strings.Contains(h.messages[3].Content, first.Rule.Chunk.Content) {
		t.Fatal("retrieval fragment was used instead of full article")
	}

	if reply.Decision != model.ReplyAnswer ||
		len(reply.Citations) != 1 ||
		reply.Citations[0].RefID != "R2" ||
		reply.Citations[0].ArticleID != "402" ||
		reply.Citations[0].SourceURL != second.Rule.Chunk.SourceURL ||
		!strings.Contains(reply.Content, "[R2]") ||
		!strings.Contains(reply.Content, "第二份虚构规则") ||
		strings.Contains(reply.Content, "[R1]") {
		t.Fatalf("unexpected citation subset: %+v", reply)
	}
	if h.workCtx.Value(valueKey{}) != "request-value" ||
		h.generationCtx.Value(valueKey{}) != "request-value" {
		t.Fatal("request context value was lost")
	}

	// 成功已返回，但两个子context都必须释放；父context保持有效。
	requireResponderContextDone(t, h.workCtx, context.Canceled)
	requireResponderContextDone(t, h.generationCtx, context.Canceled)
	if parent.Err() != nil {
		t.Fatal("responder canceled its parent")
	}

	text, err := r.Reply(parent, history, "本次测试问题")
	if err != nil || text != reply.Content {
		t.Fatalf("legacy Reply differs: text=%q error=%v", text, err)
	}
	h.requireCalls(t, 2, 2)
	if !reflect.DeepEqual(history, []llm.HistoryMessage{
		{Role: "user", Content: "旧问题"},
		{Role: "assistant", Content: "旧回答"},
	}) {
		t.Fatal("input history was mutated")
	}
}

func TestRAGResponderGenerationBranches(t *testing.T) {
	for _, upstream := range []error{
		errors.New("PRIVATE_GENERATION_ERROR"),
		context.DeadlineExceeded,
		context.Canceled,
	} {
		t.Run("upstream_"+upstream.Error(), func(t *testing.T) {
			h := newResponderHarness(t)
			// 即使同时带回看似合法的内容，也不能使用。
			h.generateErr = upstream
			r := h.responder(t, time.Second, nil)
			reply, err := r.ReplyGrounded(context.Background(), nil, "测试问题")
			requireAnswerError(t, reply, err, rag.ErrGenerationFailed)
			if err != rag.ErrGenerationFailed {
				t.Fatalf("upstream error escaped: %v", err)
			}
			h.requireCalls(t, 1, 1)
		})
	}

	invalidCases := []struct {
		name   string
		change func(*responderHarness)
	}{
		{"nil_message", func(h *responderHarness) {
			h.raw = nil
		}},
		{"invalid_json", func(h *responderHarness) {
			h.raw.Content = "PRIVATE_INVALID_MODEL_OUTPUT"
		}},
		{"unfinished_output", func(h *responderHarness) {
			h.raw.ResponseMeta.FinishReason = "length"
		}},
		{"unknown_reference", func(h *responderHarness) {
			row := normalAnswerRow()
			row.Evidence[0].RefID = "R99"
			h.raw.Content = outputJSON(t, "answer", row)
		}},
	}
	for _, tc := range invalidCases {
		t.Run(tc.name, func(t *testing.T) {
			h := newResponderHarness(t)
			tc.change(h)
			r := h.responder(t, time.Second, nil)
			reply, err := r.ReplyGrounded(context.Background(), nil, "测试问题")
			requireAnswerError(t, reply, err, rag.ErrInvalidAnswer)
			h.requireCalls(t, 1, 1)
		})
	}

	decisions := []struct {
		decision model.ReplyDecision
		content  string
	}{
		{
			model.ReplyClarify,
			"请补充具体场景、适用对象或相关时间，以便判断规则是否适用。",
		},
		{
			model.ReplyHandoff,
			"这个问题需要由官方渠道核实账号或业务信息。请通过 Bilibili 官方客服或申诉入口咨询；本项目无法代为查询或办理。",
		},
		{
			model.ReplyInsufficient,
			"本次检索资料不足以支持可靠回答。请补充规则名称或适用场景。",
		},
	}
	for _, tc := range decisions {
		t.Run(string(tc.decision), func(t *testing.T) {
			h := newResponderHarness(t)
			h.raw.Content = outputJSON(t, string(tc.decision))
			r := h.responder(t, time.Second, nil)
			reply, err := r.ReplyGrounded(context.Background(), nil, "测试问题")
			if err != nil ||
				reply.Decision != tc.decision ||
				reply.Content != tc.content ||
				reply.Citations == nil ||
				len(reply.Citations) != 0 {
				t.Fatalf("reply=%+v error=%v", reply, err)
			}
			h.requireCalls(t, 1, 1)
		})
	}
}

func TestRAGResponderParentCancellation(t *testing.T) {
	for _, stage := range []string{"retrieve", "generate"} {
		for _, withError := range []bool{false, true} {
			name := stage + "_success"
			if withError {
				name = stage + "_other_error"
			}
			t.Run(name, func(t *testing.T) {
				parent, cancel := context.WithCancel(context.Background())
				defer cancel()

				h := newResponderHarness(t)
				var dependencyErr error
				if withError {
					dependencyErr = errors.New("PRIVATE_DEPENDENCY_ERROR")
				}
				wantGenerate := 0
				if stage == "retrieve" {
					h.onRetrieve = func(
						context.Context, string, retrieval.HybridOptions,
					) (retrieval.HybridResult, error) {
						cancel()
						return h.result, dependencyErr
					}
				} else {
					wantGenerate = 1
					h.onGenerate = func(
						context.Context, []*schema.Message,
					) (*schema.Message, error) {
						cancel()
						return h.raw, dependencyErr
					}
				}

				r := h.responder(t, 2*time.Second, nil)
				reply, err := r.ReplyGrounded(parent, nil, "测试问题")
				requireAnswerError(t, reply, err, context.Canceled)
				h.requireCalls(t, 1, wantGenerate)
				requireResponderContextDone(t, h.workCtx, context.Canceled)
				if wantGenerate != 0 {
					requireResponderContextDone(t, h.generationCtx, context.Canceled)
				}
			})
		}
	}
}

func TestRAGResponderDeadlineBudget(t *testing.T) {
	cases := []struct {
		name           string
		parentTimeout  time.Duration
		modelTimeout   time.Duration
		workFromParent bool
		modelFromWork  bool
	}{
		{"background", 0, 2 * time.Second, false, false},
		{"long_parent", 60 * time.Second, 2 * time.Second, false, false},
		{"short_parent", 10 * time.Second, 2 * time.Second, true, false},
		{"model_limited_by_work", 10 * time.Second, 30 * time.Second, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parent := context.Background()
			if tc.parentTimeout != 0 {
				var cancel context.CancelFunc
				parent, cancel = context.WithTimeout(parent, tc.parentTimeout)
				defer cancel()
			}

			h := newResponderHarness(t)
			r := h.responder(t, tc.modelTimeout, nil)
			before := time.Now()
			reply, err := r.ReplyGrounded(parent, nil, "测试问题")
			requireResponderAnswer(t, reply, err)
			h.requireCalls(t, 1, 1)

			workDeadline, ok := h.workCtx.Deadline()
			if !ok {
				t.Fatal("work deadline missing")
			}
			if tc.workFromParent {
				parentDeadline, _ := parent.Deadline()
				want := parentDeadline.Add(-4 * time.Second)
				if !workDeadline.Equal(want) {
					t.Fatalf("work deadline=%v want=%v", workDeadline, want)
				}
			} else if workDeadline.Before(before.Add(36*time.Second)) ||
				workDeadline.After(h.enteredRetrieve.Add(36*time.Second)) {
				t.Fatalf("work deadline is not bounded by start+36s: %v", workDeadline)
			}

			modelDeadline, ok := h.generationCtx.Deadline()
			if !ok {
				t.Fatal("generation deadline missing")
			}
			if tc.modelFromWork {
				if !modelDeadline.Equal(workDeadline) {
					t.Fatalf("model deadline=%v work=%v", modelDeadline, workDeadline)
				}
			} else if modelDeadline.Before(before.Add(tc.modelTimeout)) ||
				modelDeadline.After(h.enteredGenerate.Add(tc.modelTimeout)) ||
				modelDeadline.After(workDeadline) {
				t.Fatalf("unexpected model deadline: %v", modelDeadline)
			}

			requireResponderContextDone(t, h.workCtx, context.Canceled)
			requireResponderContextDone(t, h.generationCtx, context.Canceled)
			if parent.Err() != nil {
				t.Fatalf("parent unexpectedly ended: %v", parent.Err())
			}
		})
	}
}

func TestRAGResponderTimeouts(t *testing.T) {
	t.Run("model_child_timeout", func(t *testing.T) {
		parent, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		h := newResponderHarness(t)
		h.onGenerate = func(
			ctx context.Context, _ []*schema.Message,
		) (*schema.Message, error) {
			<-ctx.Done()
			// 超时后的“成功内容”也必须丢弃。
			return h.raw, nil
		}
		r := h.responder(t, 150*time.Millisecond, nil)
		reply, err := r.ReplyGrounded(parent, nil, "测试问题")
		requireAnswerError(t, reply, err, rag.ErrGenerationFailed)
		h.requireCalls(t, 1, 1)
		requireResponderContextDone(t, h.generationCtx, context.DeadlineExceeded)
		requireResponderContextDone(t, h.workCtx, context.Canceled)
		if parent.Err() != nil {
			t.Fatalf("parent should remain live: %v", parent.Err())
		}
	})

	for _, stage := range []string{"retrieve", "generate"} {
		t.Run("work_timeout_during_"+stage, func(t *testing.T) {
			// 父期限保留4秒；共同工作窗口约为500毫秒。
			parent, cancel := context.WithTimeout(
				context.Background(), 4500*time.Millisecond,
			)
			defer cancel()

			h := newResponderHarness(t)
			wantGenerate := 0
			if stage == "retrieve" {
				h.onRetrieve = func(
					ctx context.Context, _ string, _ retrieval.HybridOptions,
				) (retrieval.HybridResult, error) {
					<-ctx.Done()
					return h.result, nil
				}
			} else {
				wantGenerate = 1
				h.onGenerate = func(
					ctx context.Context, _ []*schema.Message,
				) (*schema.Message, error) {
					<-ctx.Done()
					return h.raw, nil
				}
			}

			r := h.responder(t, 30*time.Second, nil)
			reply, err := r.ReplyGrounded(parent, nil, "测试问题")
			requireAnswerError(t, reply, err, context.DeadlineExceeded)
			h.requireCalls(t, 1, wantGenerate)
			requireResponderContextDone(t, h.workCtx, context.DeadlineExceeded)
			if wantGenerate != 0 {
				requireResponderContextDone(t, h.generationCtx, context.DeadlineExceeded)
			}
			if parent.Err() != nil {
				t.Fatalf("reserved parent window was lost: %v", parent.Err())
			}
		})
	}
}
