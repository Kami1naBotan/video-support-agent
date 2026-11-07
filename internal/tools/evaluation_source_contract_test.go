package tools

import (
	"context"
	"testing"

	"video-support-agent/internal/rag"
)

// Both paths use real production validation, with synthetic in-memory content.
// No request is made to the source URL.
func TestEvaluationSourceContract(t *testing.T) {
	for _, tc := range []struct {
		name, source string
		accepted     bool
	}{
		{"invalid_source", "https://example.invalid/synthetic/synthetic-appeal", false},
		{"synthetic_path_allowed_host", "https://www.bilibili.com/__evaluation_fixture__/synthetic-appeal", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newToolRig(t)
			r.current.Version.SourceURL = tc.source
			_, err := projectRuleDetail("current", r.current.Version, r.current.Articles, nil, toolAt)
			if (err == nil) != tc.accepted {
				t.Fatalf("detail accepted=%v, want %v", err == nil, tc.accepted)
			}
			r.retrieved.Items[0].Rule.Chunk.SourceURL = tc.source
			evidence, err := rag.BuildEvidence(context.Background(), r.retrieved.Items, toolAt)
			if err != nil {
				t.Fatal(err)
			}
			if (len(evidence.Rules()) > 0) != tc.accepted {
				t.Fatalf("evidence accepted=%v, want %v; stats=%+v", len(evidence.Rules()) > 0, tc.accepted, evidence.Stats())
			}
		})
	}
}
