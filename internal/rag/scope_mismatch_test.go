package rag_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"video-support-agent/internal/model"
	"video-support-agent/internal/rag"

	"go.uber.org/zap"
)

func TestRAGScopeMismatchResponder(t *testing.T) {
	// 隔离的虚构测试资料，不代表真实官方规则。
	item := fixtureItem(1)
	item.Rule.Chunk.Title = "虚构测试协议：仅适用于乙类用户"

	h := newResponderHarness(t)
	h.result = fixtureResult(item)
	h.raw = outputMessage(
		outputJSON(t, "scope_mismatch"),
	)

	reply, err := h.responder(
		t, 3*time.Second, zap.NewNop(),
	).ReplyGrounded(
		context.Background(),
		nil,
		"我是甲类用户，可以直接依据这份仅面向乙类用户的协议判断自己的权限吗？",
	)
	if err != nil {
		t.Fatal(err)
	}
	h.requireCalls(t, 1, 1)

	// 内部决定必须归一化，不能向前端泄漏新枚举。
	if reply.Decision != model.ReplyInsufficient {
		t.Fatalf("decision=%q, want insufficient", reply.Decision)
	}
	if reply.Citations == nil || len(reply.Citations) != 0 {
		t.Fatalf("unexpected citations: %+v", reply.Citations)
	}

	// 捕获误走通用澄清、遗漏范围说明或后续指引。
	for _, fragment := range []string{
		"适用范围不一致",
		"不能直接依据这些条款",
		"请查阅适用于",
		"官方客服核实",
	} {
		if !strings.Contains(reply.Content, fragment) {
			t.Fatalf("missing %q in reply: %s", fragment, reply.Content)
		}
	}
	if strings.Contains(reply.Content, "请补充具体场景、适用对象") {
		t.Fatalf("asked again for known information: %s", reply.Content)
	}
}

func TestRAGScopeMismatchRejectsInvalidPayload(t *testing.T) {
	evidence := evidenceForOutput(t)
	cases := []struct {
		name string
		raw  string
	}{
		{
			name: "nonempty_statements",
			raw: outputJSON(
				t, "scope_mismatch", normalAnswerRow(),
			),
		},
		{
			name: "extra_explanation",
			raw:  `{"decision":"scope_mismatch","statements":[],"explanation":"额外说明"}`,
		},
		{
			name: "null_statements",
			raw:  `{"decision":"scope_mismatch","statements":null}`,
		},
		{
			name: "unknown_decision",
			raw:  `{"decision":"scope_mismatch_other","statements":[]}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reply, err := rag.ValidateAnswer(
				context.Background(),
				evidence,
				outputMessage(tc.raw),
			)
			requireAnswerError(
				t, reply, err, rag.ErrInvalidAnswer,
			)
		})
	}
}
