package model_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"video-support-agent/internal/model"
)

func storedCitationFixture() model.MessageCitations {
	at := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	expiry := at.Add(24 * time.Hour)
	return model.MessageCitations{
		{
			RefID:         "R4",
			DocumentID:    "18446744073709551615",
			VersionID:     "9007199254740993",
			ArticleID:     "9007199254740995",
			CategoryID:    "1",
			ChunkID:       "9007199254740997",
			ChunkIDs:      []string{"9007199254740997", "9007199254740999"},
			Version:       "test-v1",
			Title:         "虚构历史测试规则",
			SectionPath:   "测试/历史",
			ArticleNumber: "",
			SourceURL:     "https://www.bilibili.com/__rag_fixture__",
			EffectiveAt:   at,
			ExpiresAt:     &expiry,
			RetrievedAt:   at.Add(time.Hour),
		},
	}
}

func requireCodecError(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, model.ErrInvalidMessageCitations) {
		t.Fatalf("error=%v, want controlled citation error", err)
	}
}

func TestMessageCitationsEmpty(t *testing.T) {
	for _, citations := range []model.MessageCitations{
		nil, {},
	} {
		data, err := json.Marshal(citations)
		if err != nil || string(data) != "[]" {
			t.Fatalf("empty API representation=%q err=%v", data, err)
		}
		value, err := citations.Value()
		if err != nil || value != nil {
			t.Fatalf("empty SQL representation=%v err=%v", value, err)
		}
	}

	for _, input := range []any{
		nil, "null", "[]", []byte("null"), []byte("[]"),
	} {
		got := storedCitationFixture()
		if err := got.Scan(input); err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(got)
		if err != nil || len(got) != 0 || string(data) != "[]" {
			t.Fatalf("old empty value not normalized: %q %v", data, err)
		}
	}

	var oldMessage model.Message
	data, err := json.Marshal(oldMessage)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	if string(fields["citations"]) != "[]" {
		t.Fatalf("old message omitted citations or emitted null: %s", data)
	}

	var receiver *model.MessageCitations
	requireCodecError(t, receiver.Scan(nil))
	requireCodecError(t, receiver.UnmarshalJSON([]byte("[]")))
}

func TestMessageCitationsRoundTrip(t *testing.T) {
	want := storedCitationFixture()

	if want.GormDataType() != "json" {
		t.Fatal("unexpected GORM data type")
	}
	value, err := want.Value()
	if err != nil {
		t.Fatal(err)
	}
	text, ok := value.(string)
	if !ok {
		t.Fatalf("unexpected SQL value type: %T", value)
	}

	// 两种SQL驱动常见返回类型都必须保留完整快照。
	for _, input := range []any{text, []byte(text)} {
		var got model.MessageCitations
		if err := got.Scan(input); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("SQL-interface round trip changed snapshot: %+v", got)
		}
	}

	data, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var decoded model.MessageCitations
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, want) {
		t.Fatal("JSON round trip changed snapshot")
	}
	if decoded[0].RefID != "R4" ||
		decoded[0].DocumentID != "18446744073709551615" ||
		decoded[0].ArticleNumber != "" ||
		decoded[0].ExpiresAt == nil ||
		!decoded[0].ExpiresAt.Equal(want[0].ExpiresAt.UTC()) {
		t.Fatal("historical identity, numbering, or expiry was rewritten")
	}

	messageJSON, err := json.Marshal(model.Message{
		Role:      "assistant",
		Content:   "虚构测试回答",
		Citations: want,
	})
	if err != nil {
		t.Fatal(err)
	}
	var message model.Message
	if err := json.Unmarshal(messageJSON, &message); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(message.Citations, want) {
		t.Fatal("message API did not preserve citations")
	}
}

func TestMessageCitationsCopy(t *testing.T) {
	want := storedCitationFixture()
	input := []model.RuleCitation{want[0].Clone()}
	got := model.NewMessageCitations(input)

	input[0].Title = "修改输入"
	input[0].ChunkIDs[0] = "7"
	*input[0].ExpiresAt = input[0].ExpiresAt.Add(time.Hour)

	if !reflect.DeepEqual(got, want) {
		t.Fatal("constructor retained mutable input aliases")
	}

	copyOfGot := got.Clone()
	copyOfGot[0].Title = "修改副本"
	copyOfGot[0].ChunkIDs[0] = "8"
	*copyOfGot[0].ExpiresAt = copyOfGot[0].ExpiresAt.Add(time.Hour)

	if !reflect.DeepEqual(got, want) {
		t.Fatal("Clone retained mutable aliases")
	}

	withoutExpiry := storedCitationFixture()
	withoutExpiry[0].ExpiresAt = nil
	cloned := withoutExpiry.Clone()
	if cloned[0].ExpiresAt != nil {
		t.Fatal("nil expiry was changed")
	}
}

func TestMessageCitationsDecodeErrors(t *testing.T) {
	valid, err := storedCitationFixture().MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		raw  string
	}{
		{"empty", ""},
		{"whitespace", " \n "},
		{"object", "{}"},
		{"scalar", "1"},
		{"null_element", "[null]"},
		{"unknown_field", `[{"unexpected":1}]`},
		{"wrong_id_type", `[{"document_id":123}]`},
		{"wrong_chunks_type", `[{"chunk_ids":123}]`},
		{"wrong_chunk_element", `[{"chunk_ids":["1",2]}]`},
		{"invalid_time", `[{"effective_at":"not-a-time"}]`},
		{"trailing_json", string(valid) + " {}"},
		{"incomplete_json", "["},
		{"invalid_encoding", string([]byte{'[', 0xff, ']'})},
		{
			"valid_then_invalid_element",
			string(valid[:len(valid)-1]) + ",null]",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := storedCitationFixture()
			err := got.UnmarshalJSON([]byte(tc.raw))
			requireCodecError(t, err)
			if got != nil {
				t.Fatal("UnmarshalJSON left old or partially decoded citations")
			}

			for _, input := range []any{tc.raw, []byte(tc.raw)} {
				got = storedCitationFixture()
				err = got.Scan(input)
				requireCodecError(t, err)
				if got != nil {
					t.Fatal("Scan left old or partially decoded citations")
				}
			}
		})
	}

	for _, input := range []any{42, true, struct{}{}} {
		got := storedCitationFixture()
		err := got.Scan(input)
		requireCodecError(t, err)
		if got != nil {
			t.Fatal("unsupported SQL value did not clear receiver")
		}
	}
}

func TestMessageCitationsEncodeErrors(t *testing.T) {
	fields := []struct {
		name  string
		field func(*model.RuleCitation) *string
	}{
		{"ref", func(c *model.RuleCitation) *string { return &c.RefID }},
		{"document", func(c *model.RuleCitation) *string { return &c.DocumentID }},
		{"version_id", func(c *model.RuleCitation) *string { return &c.VersionID }},
		{"article", func(c *model.RuleCitation) *string { return &c.ArticleID }},
		{"category", func(c *model.RuleCitation) *string { return &c.CategoryID }},
		{"chunk", func(c *model.RuleCitation) *string { return &c.ChunkID }},
		{"version", func(c *model.RuleCitation) *string { return &c.Version }},
		{"title", func(c *model.RuleCitation) *string { return &c.Title }},
		{"section", func(c *model.RuleCitation) *string { return &c.SectionPath }},
		{"number", func(c *model.RuleCitation) *string { return &c.ArticleNumber }},
		{"source", func(c *model.RuleCitation) *string { return &c.SourceURL }},
	}
	check := func(t *testing.T, citations model.MessageCitations) {
		t.Helper()
		data, err := citations.MarshalJSON()
		requireCodecError(t, err)
		if data != nil {
			t.Fatal("failed encoding returned bytes")
		}
		value, err := citations.Value()
		requireCodecError(t, err)
		if value != nil {
			t.Fatal("failed encoding returned a SQL value")
		}
	}

	for _, field := range fields {
		t.Run(field.name, func(t *testing.T) {
			citations := storedCitationFixture()
			*field.field(&citations[0]) = string([]byte{0xff})
			check(t, citations)
		})
	}
	t.Run("chunk_ids", func(t *testing.T) {
		citations := storedCitationFixture()
		citations[0].ChunkIDs[0] = string([]byte{0xff})
		check(t, citations)
	})

	for _, name := range []string{"effective", "expires", "retrieved"} {
		t.Run(name+"_time_outside_json_range", func(t *testing.T) {
			citations := storedCitationFixture()
			badTime := time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
			switch name {
			case "effective":
				citations[0].EffectiveAt = badTime
			case "expires":
				citations[0].ExpiresAt = &badTime
			case "retrieved":
				citations[0].RetrievedAt = badTime
			}
			check(t, citations)
		})
	}
}

func TestMessageCitationsBudget(t *testing.T) {
	const limit = 128 << 10

	citations := storedCitationFixture()
	citations[0].Title = ""
	base, err := citations.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if len(base) >= limit {
		t.Fatal("fixture unexpectedly too large")
	}

	// 这里只构造编码预算样本，不将长标题视为合法RAG证据。
	citations[0].Title = strings.Repeat("x", limit-len(base))
	exact, err := citations.MarshalJSON()
	if err != nil || len(exact) != limit {
		t.Fatalf("exact encoding boundary: bytes=%d err=%v", len(exact), err)
	}
	var decoded model.MessageCitations
	if err := decoded.Scan(exact); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, citations) {
		t.Fatal("exact-boundary snapshot was truncated")
	}

	citations[0].Title += "x"
	data, err := citations.MarshalJSON()
	requireCodecError(t, err)
	if data != nil {
		t.Fatal("oversized encoding returned partial data")
	}
	value, err := citations.Value()
	requireCodecError(t, err)
	if value != nil {
		t.Fatal("oversized encoding returned SQL value")
	}

	// 输入预算在TrimSpace之前计算，不能靠尾部空白绕过大小约束。
	over := string(exact) + " "
	for _, input := range []any{over, []byte(over)} {
		decoded = storedCitationFixture()
		err := decoded.Scan(input)
		requireCodecError(t, err)
		if decoded != nil {
			t.Fatal("oversized input retained previous citations")
		}
	}

	decoded = storedCitationFixture()
	err = decoded.UnmarshalJSON([]byte(over))
	requireCodecError(t, err)
	if decoded != nil {
		t.Fatal("oversized JSON retained previous citations")
	}
}
