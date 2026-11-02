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
	"video-support-agent/internal/rag"
	"video-support-agent/internal/retrieval"

	"github.com/cloudwego/eino/schema"
)

type fixturePrompt struct {
	Question string          `json:"question"`
	Rules    json.RawMessage `json:"rules"`
}

func readPrompt(t *testing.T, prepared rag.PreparedContext) fixturePrompt {
	t.Helper()
	if len(prepared.Messages) < 2 {
		t.Fatalf("missing model messages: %+v", prepared.Stats)
	}
	last := prepared.Messages[len(prepared.Messages)-1]
	if last == nil || last.Role != schema.User {
		t.Fatal("last message is not the current user envelope")
	}

	var result fixturePrompt
	if err := json.Unmarshal([]byte(last.Content), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func inputBytes(prepared rag.PreparedContext) int {
	total := 0
	for _, message := range prepared.Messages {
		if message != nil {
			total += len(message.Content)
		}
	}
	return total
}

func requireContextError(
	t *testing.T,
	prepared rag.PreparedContext,
	err error,
	want error,
) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("error=%v, want %v", err, want)
	}
	if !reflect.DeepEqual(prepared, rag.PreparedContext{}) {
		t.Fatalf("error returned partial context: %+v", prepared.Stats)
	}
}

func TestRAGContextEnvelope(t *testing.T) {
	item := fixtureItem(1)
	item.Rule.ArticleContent += "\n完整例外：\"单独说明\"。"
	item.Rule.Chunk.ArticleNumber = ""
	history := []llm.HistoryMessage{
		{Role: "assistant", Content: "开头孤立的旧回答"},
		{Role: "user", Content: "旧问题"},
		{Role: "assistant", Content: "旧回答及旧引用R5"},
	}

	prepared, err := rag.BuildContext(
		context.Background(),
		history,
		"  当前测试问题  ",
		fixtureResult(item),
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.FixedReason != rag.FixedNone ||
		len(prepared.Messages) != 4 ||
		prepared.Messages[0].Role != schema.System ||
		prepared.Messages[1].Role != schema.User ||
		prepared.Messages[1].Content != "旧问题" ||
		prepared.Messages[2].Role != schema.Assistant ||
		prepared.Messages[2].Content != "旧回答及旧引用R5" {
		t.Fatal("message roles or historical grouping changed")
	}

	payload := readPrompt(t, prepared)
	if payload.Question != "当前测试问题" {
		t.Fatalf("question=%q", payload.Question)
	}
	var rules []struct {
		RefID          string  `json:"ref_id"`
		DocumentID     string  `json:"document_id"`
		ArticleNumber  string  `json:"article_number"`
		ArticleContent string  `json:"article_content"`
		SourceURL      *string `json:"source_url"`
	}
	if err := json.Unmarshal(payload.Rules, &rules); err != nil {
		t.Fatal(err)
	}
	if len(rules) != 1 ||
		rules[0].RefID != "R1" ||
		rules[0].DocumentID != "201" ||
		rules[0].ArticleNumber != "" ||
		rules[0].ArticleContent != item.Rule.ArticleContent ||
		rules[0].SourceURL != nil {
		t.Fatalf("unexpected current rule envelope: %+v", rules)
	}
	if prepared.Stats.ContextCount != 1 ||
		prepared.Stats.RulesBytes != len(payload.Rules) ||
		prepared.Stats.CurrentMessageBytes != len(prepared.Messages[3].Content) ||
		prepared.Stats.InputTextBytes != inputBytes(prepared) ||
		!prepared.Stats.HistoryEvaluated ||
		prepared.Stats.HistoryInputMessages != 3 ||
		prepared.Stats.HistoryKeptMessages != 2 ||
		prepared.Stats.HistoryDroppedMessages != 1 {
		t.Fatalf("unexpected context stats: %+v", prepared.Stats)
	}

	other := fixtureItem(2)
	other.Rule.Chunk.Title = "另一份虚构测试规则"
	otherPrepared, err := rag.BuildContext(
		context.Background(), nil, "另一个问题",
		fixtureResult(other), nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if otherPrepared.Messages[0].Content != prepared.Messages[0].Content {
		t.Fatal("per-request data changed the system template")
	}

	prepared.Messages[1].Content = "修改返回值"
	if history[1].Content != "旧问题" {
		t.Fatal("prepared history shares mutable input state")
	}
}

func TestRAGContextOutcomes(t *testing.T) {
	cases := []struct {
		name     string
		outcome  retrieval.HybridOutcome
		empty    string
		degraded bool
		reason   rag.FixedReason
	}{
		{
			"no_candidates",
			retrieval.HybridNoEvidence,
			"no_candidates",
			false,
			rag.FixedNoEvidence,
		},
		{
			"current_filter",
			retrieval.HybridNoEvidence,
			"current_filter",
			false,
			rag.FixedNoEvidence,
		},
		{
			"degraded_empty",
			retrieval.HybridDegradedEmpty,
			"no_candidates",
			true,
			rag.FixedDegradedEmpty,
		},
		{
			"conflicts",
			retrieval.HybridNoUsableEvidence,
			"conflicts",
			false,
			rag.FixedUnusableEvidence,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := fixtureResult()
			result.Outcome = tc.outcome
			result.EmptyReason = tc.empty
			result.Degraded = tc.degraded
			prepared, err := rag.BuildContext(
				context.Background(), nil, "测试问题", result, nil,
			)
			if err != nil ||
				prepared.FixedReason != tc.reason ||
				len(prepared.Messages) != 0 ||
				len(prepared.Evidence.Rules()) != 0 {
				t.Fatalf("reason=%q err=%v", prepared.FixedReason, err)
			}
		})
	}

	t.Run("degraded_with_usable_evidence", func(t *testing.T) {
		result := fixtureResult(fixtureItem(1))
		result.Outcome = retrieval.HybridDegraded
		result.Degraded = true
		prepared, err := rag.BuildContext(
			context.Background(), nil, "测试问题", result, nil,
		)
		if err != nil || prepared.FixedReason != rag.FixedNone ||
			len(prepared.Messages) != 2 {
			t.Fatalf("usable degraded result rejected: %v", err)
		}
	})

	t.Run("all_metadata_excluded", func(t *testing.T) {
		item := fixtureItem(1)
		item.Rule.Chunk.SourceURL = ""
		prepared, err := rag.BuildContext(
			context.Background(), nil, "测试问题",
			fixtureResult(item), nil,
		)
		if err != nil ||
			prepared.FixedReason != rag.FixedUnusableEvidence ||
			len(prepared.Messages) != 0 ||
			prepared.Stats.Evidence.InvalidSourceCount != 1 {
			t.Fatalf("unexpected exclusion result: %+v %v", prepared, err)
		}
	})

	t.Run("question_needs_narrowing", func(t *testing.T) {
		prepared, err := rag.BuildContext(
			context.Background(), nil, strings.Repeat("问", 201),
			fixtureResult(fixtureItem(1)), nil,
		)
		if err != nil ||
			prepared.FixedReason != rag.FixedQuestionTooLong ||
			len(prepared.Messages) != 0 {
			t.Fatalf("reason=%q err=%v", prepared.FixedReason, err)
		}
	})
}

func TestRAGContextInvalidResults(t *testing.T) {
	cases := []struct {
		name   string
		change func(*retrieval.HybridResult)
	}{
		{"nil_items", func(r *retrieval.HybridResult) {
			r.Items = nil
		}},
		{"wrong_final_count", func(r *retrieval.HybridResult) {
			r.Stats.FinalCount = 2
		}},
		{"failure_marked_as_success", func(r *retrieval.HybridResult) {
			r.ErrorKind = "backend"
		}},
		{"unknown_outcome", func(r *retrieval.HybridResult) {
			r.Outcome = retrieval.HybridOutcome("unknown")
		}},
		{"nonempty_marked_empty", func(r *retrieval.HybridResult) {
			r.Outcome = retrieval.HybridNoEvidence
			r.EmptyReason = "no_candidates"
		}},
		{"wrong_degraded_flag", func(r *retrieval.HybridResult) {
			r.Degraded = true
		}},
		{"unexpected_empty_reason", func(r *retrieval.HybridResult) {
			r.EmptyReason = "no_candidates"
		}},
		{"missing_retrieval_time", func(r *retrieval.HybridResult) {
			r.RetrievedAt = time.Time{}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := fixtureResult(fixtureItem(1))
			tc.change(&result)
			prepared, err := rag.BuildContext(
				context.Background(), nil, "测试问题", result, nil,
			)
			requireContextError(t, prepared, err, rag.ErrInvalidEvidence)
		})
	}

	t.Run("retrieval_error_is_not_no_evidence", func(t *testing.T) {
		sentinel := errors.New("controlled retrieval failure")
		prepared, err := rag.BuildContext(
			context.Background(), nil, "测试问题",
			fixtureResult(fixtureItem(1)), sentinel,
		)
		requireContextError(t, prepared, err, sentinel)
	})
}

func TestRAGContextRulesBudget(t *testing.T) {
	t.Run("exact_limit_and_one_byte_over", func(t *testing.T) {
		item := fixtureItem(1)
		item.Rule.ArticleContent = "X"
		base, err := rag.BuildContext(
			context.Background(), nil, "测试问题",
			fixtureResult(item), nil,
		)
		if err != nil {
			t.Fatal(err)
		}

		// 从实际输出的JSON测量固定开销，不只相信Stats。
		baseRulesBytes := len(readPrompt(t, base).Rules)
		padding := (24 << 10) - baseRulesBytes
		if padding <= 0 {
			t.Fatalf("fixture unexpectedly exceeds budget: %d", baseRulesBytes)
		}
		item.Rule.ArticleContent = strings.Repeat("X", 1+padding)

		exact, err := rag.BuildContext(
			context.Background(), nil, "测试问题",
			fixtureResult(item), nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		actualBytes := len(readPrompt(t, exact).Rules)
		if exact.FixedReason != rag.FixedNone ||
			actualBytes != 24<<10 ||
			exact.Stats.RulesBytes != actualBytes ||
			exact.Evidence.Rules()[0].ArticleContent != item.Rule.ArticleContent {
			t.Fatalf("exact rule boundary failed: %+v", exact.Stats)
		}

		item.Rule.ArticleContent += "X"
		over, err := rag.BuildContext(
			context.Background(), nil, "测试问题",
			fixtureResult(item), nil,
		)
		if err != nil ||
			over.FixedReason != rag.FixedContextTooLarge ||
			over.Stats.RulesBytes != (24<<10)+1 ||
			len(over.Messages) != 0 ||
			len(over.Evidence.Rules()) != 0 {
			t.Fatalf("oversized rules were not rejected whole: %+v %v", over, err)
		}
	})

	t.Run("serialized_bytes_not_raw_text_bytes", func(t *testing.T) {
		item := fixtureItem(1)
		// JSON编码会转义这些普通字符，原文长度远低于24KiB。
		item.Rule.ArticleContent = strings.Repeat("<", 4100)
		prepared, err := rag.BuildContext(
			context.Background(), nil, "测试问题",
			fixtureResult(item), nil,
		)
		if err != nil ||
			prepared.FixedReason != rag.FixedContextTooLarge ||
			prepared.Stats.RulesBytes <= 24<<10 ||
			len(prepared.Messages) != 0 {
			t.Fatalf("serialized budget not enforced: %+v %v", prepared, err)
		}
	})
}

func TestRAGContextHistory(t *testing.T) {
	build := func(t *testing.T, history []llm.HistoryMessage) rag.PreparedContext {
		t.Helper()
		prepared, err := rag.BuildContext(
			context.Background(), history, "测试问题",
			fixtureResult(fixtureItem(1)), nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		if prepared.FixedReason != rag.FixedNone ||
			inputBytes(prepared) > 32<<10 ||
			prepared.Stats.InputTextBytes != inputBytes(prepared) {
			t.Fatalf("invalid prepared history: %+v", prepared.Stats)
		}
		return prepared
	}

	t.Run("latest_contiguous_groups_only", func(t *testing.T) {
		history := []llm.HistoryMessage{
			{Role: "user", Content: "最老问题"},
			{Role: "assistant", Content: "最老回答"},
			{Role: "user", Content: strings.Repeat("x", 32<<10)},
			{Role: "assistant", Content: "中间回答"},
			{Role: "user", Content: "最新问题"},
			{Role: "assistant", Content: "最新回答"},
		}
		prepared := build(t, history)
		if len(prepared.Messages) != 4 ||
			prepared.Messages[1].Content != "最新问题" ||
			prepared.Messages[2].Content != "最新回答" ||
			prepared.Stats.HistoryKeptMessages != 2 ||
			prepared.Stats.HistoryDroppedMessages != 4 {
			t.Fatalf("history skipped across an oversized group: %+v", prepared.Stats)
		}
	})

	t.Run("oversized_latest_group_keeps_no_older_group", func(t *testing.T) {
		prepared := build(t, []llm.HistoryMessage{
			{Role: "user", Content: "旧问题"},
			{Role: "assistant", Content: "旧回答"},
			{Role: "user", Content: strings.Repeat("x", 32<<10)},
		})
		if len(prepared.Messages) != 2 ||
			prepared.Stats.HistoryKeptMessages != 0 ||
			prepared.Stats.HistoryDroppedMessages != 3 {
			t.Fatal("an older group was retained across an oversized latest group")
		}
	})

	t.Run("exact_total_budget", func(t *testing.T) {
		base := build(t, nil)
		room := (32 << 10) - inputBytes(base)
		if room <= 0 {
			t.Fatal("fixture has no room for history")
		}
		history := []llm.HistoryMessage{
			{Role: "user", Content: strings.Repeat("h", room)},
		}
		exact := build(t, history)
		if inputBytes(exact) != 32<<10 ||
			exact.Stats.HistoryKeptMessages != 1 ||
			exact.Messages[1].Content != history[0].Content {
			t.Fatal("exact input boundary or complete history was lost")
		}

		history[0].Content += "h"
		over := build(t, history)
		if len(over.Messages) != 2 ||
			over.Stats.HistoryKeptMessages != 0 ||
			over.Stats.HistoryDroppedMessages != 1 {
			t.Fatal("oversized history was truncated instead of dropping its group")
		}
	})

	t.Run("orphan_assistant_is_removed", func(t *testing.T) {
		prepared := build(t, []llm.HistoryMessage{
			{Role: "assistant", Content: "孤立回答"},
		})
		if len(prepared.Messages) != 2 ||
			prepared.Stats.HistoryDroppedMessages != 1 {
			t.Fatal("orphan assistant retained")
		}
	})

	for _, tc := range []struct {
		name string
		item llm.HistoryMessage
	}{
		{"invalid_role", llm.HistoryMessage{Role: "tool", Content: "测试"}},
		{"blank_content", llm.HistoryMessage{Role: "user", Content: " \n "}},
		{"invalid_encoding", llm.HistoryMessage{
			Role: "assistant", Content: string([]byte{0xff}),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prepared, err := rag.BuildContext(
				context.Background(),
				[]llm.HistoryMessage{tc.item},
				"测试问题",
				fixtureResult(fixtureItem(1)),
				nil,
			)
			requireContextError(t, prepared, err, rag.ErrInvalidHistory)
		})
	}
}

func TestRAGContextCancellation(t *testing.T) {
	t.Run("canceled_before_other_errors", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		prepared, err := rag.BuildContext(
			ctx, nil, "",
			retrieval.HybridResult{},
			errors.New("another error"),
		)
		requireContextError(t, prepared, err, context.Canceled)
	})

	t.Run("expired_deadline", func(t *testing.T) {
		ctx, cancel := context.WithDeadline(
			context.Background(), time.Now().Add(-time.Second),
		)
		defer cancel()
		prepared, err := rag.BuildContext(
			ctx, nil, "测试问题",
			fixtureResult(fixtureItem(1)), nil,
		)
		requireContextError(t, prepared, err, context.DeadlineExceeded)
	})
}
