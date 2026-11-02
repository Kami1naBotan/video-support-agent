package rag_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"video-support-agent/internal/model"
	"video-support-agent/internal/rag"
	"video-support-agent/internal/retrieval"

	"github.com/cloudwego/eino/schema"
)

const fixtureQuote = "满足条件A时可提交材料"

type answerProof struct {
	RefID string `json:"ref_id"`
	Quote string `json:"quote"`
}

type answerRow struct {
	Text     string        `json:"text"`
	Evidence []answerProof `json:"evidence"`
}

func outputJSON(t *testing.T, decision string, rows ...answerRow) string {
	t.Helper()
	if rows == nil {
		rows = []answerRow{}
	}
	data, err := json.Marshal(struct {
		Decision   string      `json:"decision"`
		Statements []answerRow `json:"statements"`
	}{
		Decision:   decision,
		Statements: rows,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func outputMessage(raw string) *schema.Message {
	return &schema.Message{
		Role:    schema.Assistant,
		Content: raw,
		ResponseMeta: &schema.ResponseMeta{
			FinishReason: "stop",
		},
	}
}

func evidenceForOutput(
	t *testing.T,
	items ...retrieval.HybridItem,
) rag.EvidenceSet {
	t.Helper()
	if len(items) == 0 {
		items = []retrieval.HybridItem{fixtureItem(1)}
	}
	evidence, err := rag.BuildEvidence(
		context.Background(), items, fixtureAt,
	)
	if err != nil {
		t.Fatal(err)
	}
	return evidence
}

func normalAnswerRow() answerRow {
	return answerRow{
		Text: "测试陈述",
		Evidence: []answerProof{
			{RefID: "R1", Quote: fixtureQuote},
		},
	}
}

func requireAnswerError(
	t *testing.T,
	reply model.GroundedReply,
	err error,
	want error,
) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("error=%v, want %v", err, want)
	}
	if !reflect.DeepEqual(reply, model.GroundedReply{}) {
		t.Fatalf("error returned partial reply: %+v", reply)
	}
}

func TestRAGAnswerRenderAndSnapshot(t *testing.T) {
	a, b := fixtureItem(1), fixtureItem(2)
	a.Rule.Chunk.Title = "虚构测试甲"
	b.Rule.Chunk.Title = "虚构测试乙"
	b.Rule.Chunk.ArticleNumber = ""
	b.Rule.Chunk.SourceURL += "?case=2"
	expiry := fixtureAt.Add(time.Hour)
	b.Rule.Chunk.ExpiresAt = &expiry

	evidence := evidenceForOutput(t, a, b)
	raw := outputJSON(t, "answer",
		answerRow{
			Text:     "先说明乙",
			Evidence: []answerProof{{RefID: "R2", Quote: fixtureQuote}},
		},
		answerRow{
			Text: "再说明甲乙",
			Evidence: []answerProof{
				{RefID: "R1", Quote: fixtureQuote},
				{RefID: "R2", Quote: fixtureQuote},
			},
		},
	)
	message := outputMessage(raw)
	message.ReasoningContent = "INTERNAL_TEST_MARKER"

	reply, err := rag.ValidateAnswer(context.Background(), evidence, message)
	if err != nil {
		t.Fatal(err)
	}

	wantContent := `1. 先说明乙 [R2]
2. 再说明甲乙 [R1] [R2]

依据（检索时快照）：
[R1] 规则："虚构测试甲"；条款："TEST-1"；版本："test-v1"
来源："https://www.bilibili.com/__rag_fixture__"
[R2] 规则："虚构测试乙"；条款："原文未编号"；版本："test-v1"
来源："https://www.bilibili.com/__rag_fixture__?case=2"`

	if reply.Decision != model.ReplyAnswer || reply.Content != wantContent {
		t.Fatalf("unexpected rendered reply:\n%s", reply.Content)
	}
	if len(reply.Citations) != 2 ||
		reply.Citations[0].RefID != "R1" ||
		reply.Citations[1].RefID != "R2" {
		t.Fatalf("unexpected citations: %+v", reply.Citations)
	}
	if strings.Contains(reply.Content, "INTERNAL_TEST_MARKER") ||
		message.Content != raw {
		t.Fatal("extra model metadata leaked or input was changed")
	}

	// 单独使用R2时不能重新编号为R1，也不能附带未使用的R1。
	subsetRaw := outputJSON(t, "answer", answerRow{
		Text:     "仅说明乙",
		Evidence: []answerProof{{RefID: "R2", Quote: fixtureQuote}},
	})
	subset, err := rag.ValidateAnswer(
		context.Background(), evidence, outputMessage(subsetRaw),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(subset.Citations) != 1 ||
		subset.Citations[0].RefID != "R2" ||
		subset.Citations[0].ArticleNumber != "" ||
		strings.Contains(subset.Content, "[R1]") {
		t.Fatalf("reference subset changed: %+v", subset)
	}

	subset.Citations[0].ChunkIDs[0] = "999"
	*subset.Citations[0].ExpiresAt = fixtureAt.Add(20 * time.Hour)

	again, err := rag.ValidateAnswer(
		context.Background(), evidence, outputMessage(subsetRaw),
	)
	if err != nil {
		t.Fatal(err)
	}
	if again.Citations[0].ChunkIDs[0] != "102" ||
		!again.Citations[0].ExpiresAt.Equal(fixtureAt.Add(time.Hour)) {
		t.Fatal("returned citation shares mutable evidence")
	}
}

func TestRAGAnswerDecisions(t *testing.T) {
	evidence := evidenceForOutput(t)
	cases := []struct {
		decision model.ReplyDecision
		text     string
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
	for _, tc := range cases {
		t.Run(string(tc.decision), func(t *testing.T) {
			reply, err := rag.ValidateAnswer(
				context.Background(),
				evidence,
				outputMessage(outputJSON(t, string(tc.decision))),
			)
			if err != nil ||
				reply.Decision != tc.decision ||
				reply.Content != tc.text ||
				reply.Citations == nil ||
				len(reply.Citations) != 0 {
				t.Fatalf("reply=%+v err=%v", reply, err)
			}

			reply, err = rag.ValidateAnswer(
				context.Background(),
				evidence,
				outputMessage(outputJSON(t, string(tc.decision), normalAnswerRow())),
			)
			requireAnswerError(t, reply, err, rag.ErrInvalidAnswer)
		})
	}

	fixed := []struct {
		reason   rag.FixedReason
		decision model.ReplyDecision
	}{
		{rag.FixedQuestionTooLong, model.ReplyClarify},
		{rag.FixedContextTooLarge, model.ReplyClarify},
		{rag.FixedNoEvidence, model.ReplyInsufficient},
		{rag.FixedDegradedEmpty, model.ReplyInsufficient},
		{rag.FixedUnusableEvidence, model.ReplyInsufficient},
	}
	for _, tc := range fixed {
		t.Run("fixed_"+string(tc.reason), func(t *testing.T) {
			reply, err := rag.FixedReply(context.Background(), tc.reason)
			if err != nil {
				t.Fatal(err)
			}
			// FixedText的逐字文案已经由上一块独立测试覆盖。
			text, err := rag.FixedText(tc.reason)
			if err != nil ||
				reply.Decision != tc.decision ||
				reply.Content != text ||
				reply.Citations == nil ||
				len(reply.Citations) != 0 {
				t.Fatalf("reply=%+v err=%v", reply, err)
			}
		})
	}

	reply, err := rag.FixedReply(context.Background(), rag.FixedNone)
	requireAnswerError(t, reply, err, rag.ErrInvalidPreparation)
}

func TestRAGAnswerEnvelope(t *testing.T) {
	evidence := evidenceForOutput(t)
	raw := outputJSON(t, "answer", normalAnswerRow())
	cases := []struct {
		name   string
		change func(*schema.Message)
	}{
		{"wrong_role", func(m *schema.Message) {
			m.Role = schema.User
		}},
		{"missing_metadata", func(m *schema.Message) {
			m.ResponseMeta = nil
		}},
		{"missing_finish_reason", func(m *schema.Message) {
			m.ResponseMeta.FinishReason = ""
		}},
		{"truncated", func(m *schema.Message) {
			m.ResponseMeta.FinishReason = "length"
		}},
		{"unknown_finish_reason", func(m *schema.Message) {
			m.ResponseMeta.FinishReason = "unknown"
		}},
		{"tool_calls", func(m *schema.Message) {
			m.ToolCalls = []schema.ToolCall{{}}
		}},
		{"tool_call_id", func(m *schema.Message) {
			m.ToolCallID = "test-call"
		}},
		{"tool_name", func(m *schema.Message) {
			m.ToolName = "test-tool"
		}},
		{"legacy_multi_content", func(m *schema.Message) {
			m.MultiContent = []schema.ChatMessagePart{{}}
		}},
		{"user_multi_content", func(m *schema.Message) {
			m.UserInputMultiContent = []schema.MessageInputPart{{}}
		}},
		{"assistant_multi_content", func(m *schema.Message) {
			m.AssistantGenMultiContent = []schema.MessageOutputPart{{}}
		}},
		{"empty_content", func(m *schema.Message) {
			m.Content = ""
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			message := outputMessage(raw)
			tc.change(message)
			reply, err := rag.ValidateAnswer(context.Background(), evidence, message)
			requireAnswerError(t, reply, err, rag.ErrInvalidAnswer)
		})
	}

	reply, err := rag.ValidateAnswer(context.Background(), evidence, nil)
	requireAnswerError(t, reply, err, rag.ErrInvalidAnswer)

	reply, err = rag.ValidateAnswer(
		context.Background(), rag.EvidenceSet{}, outputMessage(raw),
	)
	requireAnswerError(t, reply, err, rag.ErrInvalidEvidence)
}

func TestRAGAnswerJSONContract(t *testing.T) {
	evidence := evidenceForOutput(t)
	wrapRow := func(row string) string {
		return `{"decision":"answer","statements":[` + row + `]}`
	}
	wrapProof := func(proof string) string {
		return wrapRow(`{"text":"测试陈述","evidence":[` + proof + `]}`)
	}

	cases := []struct {
		name string
		raw  string
	}{
		{"top_null", `null`},
		{"top_array", `[]`},
		{"missing_fields", `{}`},
		{"missing_decision", `{"statements":[]}`},
		{"missing_statements", `{"decision":"clarify"}`},
		{"unknown_field", `{"decision":"clarify","statements":[],"extra":1}`},
		{"duplicate_field", `{"decision":"clarify","decision":"clarify","statements":[]}`},
		{"escaped_duplicate", `{"decision":"clarify","deci\u0073ion":"clarify","statements":[]}`},
		{"decision_wrong_type", `{"decision":1,"statements":[]}`},
		{"unknown_decision", `{"decision":"other","statements":[]}`},
		{"null_statements", `{"decision":"clarify","statements":null}`},
		{"object_statements", `{"decision":"clarify","statements":{}}`},
		{"empty_answer", `{"decision":"answer","statements":[]}`},
		{"trailing_object", `{"decision":"clarify","statements":[]} {}`},
		{"incomplete_json", `{"decision":`},
		{"markdown_wrapper", "```json\n{\"decision\":\"clarify\",\"statements\":[]}\n```"},
		{"null_row", wrapRow(`null`)},
		{"missing_text", wrapRow(`{"evidence":[]}`)},
		{"missing_evidence", wrapRow(`{"text":"测试陈述"}`)},
		{"null_evidence", wrapRow(`{"text":"测试陈述","evidence":null}`)},
		{"wrong_text_type", wrapRow(`{"text":1,"evidence":[]}`)},
		{"extra_row_field", wrapRow(`{"text":"测试","evidence":[],"extra":1}`)},
		{"duplicate_text", wrapRow(`{"text":"甲","text":"乙","evidence":[]}`)},
		{"null_proof", wrapProof(`null`)},
		{"missing_ref", wrapProof(`{"quote":"满足条件A时可提交材料"}`)},
		{"missing_quote", wrapProof(`{"ref_id":"R1"}`)},
		{"wrong_ref_type", wrapProof(`{"ref_id":1,"quote":"满足条件A时可提交材料"}`)},
		{"wrong_quote_type", wrapProof(`{"ref_id":"R1","quote":1}`)},
		{"extra_proof_field", wrapProof(`{"ref_id":"R1","quote":"满足条件A时可提交材料","extra":1}`)},
		{"duplicate_ref", wrapProof(`{"ref_id":"R1","ref_id":"R1","quote":"满足条件A时可提交材料"}`)},
		{"duplicate_quote", wrapProof(`{"ref_id":"R1","quote":"甲","quote":"乙"}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reply, err := rag.ValidateAnswer(
				context.Background(), evidence, outputMessage(tc.raw),
			)
			requireAnswerError(t, reply, err, rag.ErrInvalidAnswer)
		})
	}

	t.Run("field_order_is_not_significant", func(t *testing.T) {
		raw := `{"statements":[{"evidence":[{"quote":"满足条件A时可提交材料","ref_id":"R1"}],"text":"测试陈述"}],"decision":"answer"}`
		reply, err := rag.ValidateAnswer(
			context.Background(), evidence, outputMessage(raw),
		)
		if err != nil || reply.Decision != model.ReplyAnswer {
			t.Fatalf("valid reordered JSON rejected: %v", err)
		}
	})
}

func TestRAGAnswerGrounding(t *testing.T) {
	a, b := fixtureItem(1), fixtureItem(2)
	b.Rule.ArticleContent = "虚构乙条款：仅适用乙条件。"
	evidence := evidenceForOutput(t, a, b)

	cases := []struct {
		name string
		row  answerRow
	}{
		{"unknown_ref", answerRow{
			Text: "测试陈述",
			Evidence: []answerProof{
				{RefID: "R3", Quote: fixtureQuote},
			},
		}},
		{"quote_from_wrong_article", answerRow{
			Text: "测试陈述",
			Evidence: []answerProof{
				{RefID: "R1", Quote: "仅适用乙条件"},
			},
		}},
		{"modified_quote", answerRow{
			Text: "测试陈述",
			Evidence: []answerProof{
				{RefID: "R1", Quote: "满足条件A时可提交其它材料"},
			},
		}},
		{"empty_evidence", answerRow{
			Text: "测试陈述", Evidence: []answerProof{},
		}},
		{"blank_quote", answerRow{
			Text: "测试陈述",
			Evidence: []answerProof{
				{RefID: "R1", Quote: " "},
			},
		}},
		{"repeated_ref_in_one_statement", answerRow{
			Text: "测试陈述",
			Evidence: []answerProof{
				{RefID: "R1", Quote: fixtureQuote},
				{RefID: "R1", Quote: fixtureQuote},
			},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// 第一条有效、第二条无效，也必须整体失败。
			raw := outputJSON(t, "answer", normalAnswerRow(), tc.row)
			reply, err := rag.ValidateAnswer(
				context.Background(), evidence, outputMessage(raw),
			)
			requireAnswerError(t, reply, err, rag.ErrInvalidAnswer)
		})
	}

	for _, text := range []string{
		" ", "包含[R1]", "包含换行\n内容", "包含制表\t内容",
		"https://example.org", "WWW.example.org",
	} {
		t.Run("invalid_text_"+text, func(t *testing.T) {
			row := normalAnswerRow()
			row.Text = text
			reply, err := rag.ValidateAnswer(
				context.Background(),
				evidence,
				outputMessage(outputJSON(t, "answer", row)),
			)
			requireAnswerError(t, reply, err, rag.ErrInvalidAnswer)
		})
	}

	row := normalAnswerRow()
	row.Text = "  测试陈述  "
	reply, err := rag.ValidateAnswer(
		context.Background(), evidence,
		outputMessage(outputJSON(t, "answer", row)),
	)
	if err != nil || !strings.HasPrefix(reply.Content, "1. 测试陈述 [R1]\n") {
		t.Fatalf("plain text normalization failed: %+v %v", reply, err)
	}
}

func TestRAGAnswerLimits(t *testing.T) {
	check := func(
		t *testing.T,
		evidence rag.EvidenceSet,
		raw string,
		valid bool,
	) {
		t.Helper()
		reply, err := rag.ValidateAnswer(
			context.Background(), evidence, outputMessage(raw),
		)
		if !valid {
			requireAnswerError(t, reply, err, rag.ErrInvalidAnswer)
			return
		}
		if err != nil || reply.Decision != model.ReplyAnswer ||
			len(reply.Citations) == 0 {
			t.Fatalf("valid boundary rejected: %+v %v", reply, err)
		}
	}

	t.Run("statement_count", func(t *testing.T) {
		evidence := evidenceForOutput(t)
		rows := make([]answerRow, 8)
		for i := range rows {
			rows[i] = normalAnswerRow()
		}
		check(t, evidence, outputJSON(t, "answer", rows...), true)
		rows = append(rows, normalAnswerRow())
		check(t, evidence, outputJSON(t, "answer", rows...), false)
	})

	t.Run("statement_unicode_length", func(t *testing.T) {
		evidence := evidenceForOutput(t)
		row := normalAnswerRow()
		row.Text = strings.Repeat("字", 1000)
		check(t, evidence, outputJSON(t, "answer", row), true)
		row.Text += "字"
		check(t, evidence, outputJSON(t, "answer", row), false)
	})

	t.Run("quote_unicode_length", func(t *testing.T) {
		item := fixtureItem(1)
		item.Rule.ArticleContent = strings.Repeat("文", 501)
		evidence := evidenceForOutput(t, item)
		row := normalAnswerRow()
		row.Evidence[0].Quote = strings.Repeat("文", 500)
		check(t, evidence, outputJSON(t, "answer", row), true)
		row.Evidence[0].Quote += "文"
		check(t, evidence, outputJSON(t, "answer", row), false)
	})

	t.Run("five_distinct_references", func(t *testing.T) {
		evidence := evidenceForOutput(t,
			fixtureItem(1), fixtureItem(2), fixtureItem(3),
			fixtureItem(4), fixtureItem(5),
		)
		row := normalAnswerRow()
		row.Evidence = []answerProof{
			{RefID: "R1", Quote: fixtureQuote},
			{RefID: "R2", Quote: fixtureQuote},
			{RefID: "R3", Quote: fixtureQuote},
			{RefID: "R4", Quote: fixtureQuote},
			{RefID: "R5", Quote: fixtureQuote},
		}
		check(t, evidence, outputJSON(t, "answer", row), true)
		row.Evidence = append(row.Evidence, row.Evidence[0])
		check(t, evidence, outputJSON(t, "answer", row), false)
	})

	t.Run("raw_bytes_exact_and_over", func(t *testing.T) {
		evidence := evidenceForOutput(t)
		raw := outputJSON(t, "answer", normalAnswerRow())
		if len(raw) >= 16<<10 {
			t.Fatal("fixture unexpectedly too large")
		}
		raw += strings.Repeat(" ", (16<<10)-len(raw))
		check(t, evidence, raw, true)
		check(t, evidence, raw+" ", false)
	})
}

func TestRAGAnswerUnicode(t *testing.T) {
	item := fixtureItem(1)
	item.Rule.ArticleContent = "虚构测试：😀可以出现在原文中。"
	evidence := evidenceForOutput(t, item)

	literal := `{"decision":"answer","statements":[{"text":"😀","evidence":[{"ref_id":"R1","quote":"😀"}]}]}`
	escaped := `{"decision":"answer","statements":[{"text":"\uD83D\uDE00","evidence":[{"ref_id":"R1","quote":"\uD83D\uDE00"}]}]}`

	first, err := rag.ValidateAnswer(
		context.Background(), evidence, outputMessage(literal),
	)
	if err != nil {
		t.Fatal(err)
	}
	second, err := rag.ValidateAnswer(
		context.Background(), evidence, outputMessage(escaped),
	)
	if err != nil || !reflect.DeepEqual(first, second) {
		t.Fatalf("equivalent Unicode representations differ: %v", err)
	}

	for _, unit := range []string{`\uD800`, `\uDC00`, `\uD800\u0041`} {
		t.Run(unit, func(t *testing.T) {
			raw := `{"decision":"answer","statements":[{"text":"` +
				unit + `","evidence":[{"ref_id":"R1","quote":"😀"}]}]}`
			reply, err := rag.ValidateAnswer(
				context.Background(), evidence, outputMessage(raw),
			)
			requireAnswerError(t, reply, err, rag.ErrInvalidAnswer)
		})
	}

	raw := strings.Replace(literal, `"text":"😀"`,
		`"text":"`+string([]byte{0xff})+`"`, 1)
	reply, err := rag.ValidateAnswer(
		context.Background(), evidence, outputMessage(raw),
	)
	requireAnswerError(t, reply, err, rag.ErrInvalidAnswer)
}

func TestRAGAnswerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	reply, err := rag.ValidateAnswer(ctx, rag.EvidenceSet{}, nil)
	requireAnswerError(t, reply, err, context.Canceled)

	reply, err = rag.FixedReply(ctx, rag.FixedNone)
	requireAnswerError(t, reply, err, context.Canceled)

	ctx, cancel = context.WithDeadline(
		context.Background(), time.Now().Add(-time.Second),
	)
	defer cancel()

	reply, err = rag.ValidateAnswer(
		ctx,
		evidenceForOutput(t),
		outputMessage(outputJSON(t, "answer", normalAnswerRow())),
	)
	requireAnswerError(t, reply, err, context.DeadlineExceeded)
}
