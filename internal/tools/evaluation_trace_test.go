package tools

import (
	"context"
	"strings"
	"testing"
)

func TestEvaluationTraceContract(t *testing.T) {
	recordEvaluationInvocation(context.Background(), SearchRuleName, `{"query":"synthetic"}`)
	t.Run("capture_and_consume", func(t *testing.T) {
		ctx, tr, e := NewEvaluationTrace(context.Background())
		if e != nil {
			t.Fatal(e)
		}
		ctx = WithEvaluationCall(ctx, EvaluationCallIdentity{RunID: "11111111-1111-4111-8111-111111111111", CallID: "22222222-2222-4222-8222-222222222222", Sequence: 1})
		recordEvaluationInvocation(ctx, SearchRuleName, `{"query":"synthetic"}`)
		rows, e := tr.Take()
		if e != nil || len(rows) != 1 || rows[0].Name != SearchRuleName || rows[0].Arguments != `{"query":"synthetic"}` {
			t.Fatalf("capture failed %v", e)
		}
		if _, e = tr.Take(); e == nil {
			t.Fatal("trace consumed twice")
		}
	})
	for _, name := range []string{"missing_identity", "duplicate", "oversized", "unknown_tool"} {
		t.Run(name, func(t *testing.T) {
			ctx, tr, e := NewEvaluationTrace(context.Background())
			if e != nil {
				t.Fatal(e)
			}
			if name != "missing_identity" {
				ctx = WithEvaluationCall(ctx, EvaluationCallIdentity{RunID: "11111111-1111-4111-8111-111111111111", CallID: "22222222-2222-4222-8222-222222222222", Sequence: 1})
			}
			tool, raw := SearchRuleName, `{}`
			if name == "unknown_tool" {
				tool = "not-a-tool"
			}
			if name == "oversized" {
				raw = strings.Repeat("x", MaxArgumentBytes+1)
			}
			recordEvaluationInvocation(ctx, tool, raw)
			if name == "duplicate" {
				recordEvaluationInvocation(ctx, tool, raw)
			}
			rows, e := tr.Take()
			if e == nil || rows != nil {
				t.Fatal("invalid trace treated as complete")
			}
		})
	}
}
