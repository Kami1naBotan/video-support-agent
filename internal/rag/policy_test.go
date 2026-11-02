package rag_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"video-support-agent/internal/llm"
	"video-support-agent/internal/rag"
)

func TestRAGQuestionPolicy(t *testing.T) {
	cases := []struct {
		name   string
		raw    string
		query  string
		reason rag.FixedReason
		err    error
	}{
		{
			name:  "trim",
			raw:   " \n测试问题\t ",
			query: "测试问题",
		},
		{
			name:  "200_unicode_characters",
			raw:   strings.Repeat("问", 200),
			query: strings.Repeat("问", 200),
		},
		{
			name:   "201_characters",
			raw:    strings.Repeat("问", 201),
			reason: rag.FixedQuestionTooLong,
		},
		{
			name:   "10000_characters",
			raw:    strings.Repeat("问", 10000),
			reason: rag.FixedQuestionTooLong,
		},
		{
			name: "10001_characters",
			raw:  strings.Repeat("问", 10001),
			err:  llm.ErrInvalidInput,
		},
		{
			name: "blank",
			raw:  " \n\t",
			err:  llm.ErrInvalidInput,
		},
		{
			name: "invalid_encoding",
			raw:  string([]byte{0xff}),
			err:  llm.ErrInvalidInput,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			query, reason, err := rag.PrepareQuestion(
				context.Background(), tc.raw,
			)
			if !errors.Is(err, tc.err) ||
				query != tc.query ||
				reason != tc.reason {
				t.Fatalf(
					"query=%q reason=%q err=%v",
					query, reason, err,
				)
			}
		})
	}

	t.Run("cancellation_has_priority", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		query, reason, err := rag.PrepareQuestion(ctx, "")
		if !errors.Is(err, context.Canceled) ||
			query != "" || reason != rag.FixedNone {
			t.Fatalf("query=%q reason=%q err=%v", query, reason, err)
		}
	})
}

func TestRAGFixedText(t *testing.T) {
	cases := []struct {
		reason rag.FixedReason
		text   string
	}{
		{
			rag.FixedQuestionTooLong,
			"请将问题提炼为200字以内，并说明适用对象或具体场景。",
		},
		{
			rag.FixedContextTooLarge,
			"相关规则的完整内容超出本次处理范围。请缩小问题范围，例如明确规则名称、条款或适用场景。",
		},
		{
			rag.FixedNoEvidence,
			"当前知识库未找到足够依据回答这个问题。请补充规则名称或具体场景；这不表示平台没有相关规则。",
		},
		{
			rag.FixedDegradedEmpty,
			"当前部分检索服务不可用，暂未找到可核实的依据。请稍后重试，或补充规则名称与具体场景。",
		},
		{
			rag.FixedUnusableEvidence,
			"当前检索资料无法形成可核实的依据，暂不能据此回答。请补充规则名称或具体场景。",
		},
	}
	for _, tc := range cases {
		t.Run(string(tc.reason), func(t *testing.T) {
			got, err := rag.FixedText(tc.reason)
			if err != nil || got != tc.text {
				t.Fatalf("text=%q err=%v", got, err)
			}
		})
	}

	for _, reason := range []rag.FixedReason{
		rag.FixedNone, rag.FixedReason("unknown"),
	} {
		got, err := rag.FixedText(reason)
		if !errors.Is(err, rag.ErrInvalidPreparation) || got != "" {
			t.Fatalf("unknown reason accepted: text=%q err=%v", got, err)
		}
	}
}
