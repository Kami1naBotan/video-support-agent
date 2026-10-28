package service

import (
	"context"
	"errors"
	"testing"
)

func TestKnowledgeServiceCreateDraftRejectsBlankDocumentKey(t *testing.T) {
	tests := []struct {
		name        string
		documentKey string
	}{
		{name: "empty", documentKey: ""},
		{name: "spaces", documentKey: "   "},
		{name: "whitespace", documentKey: "\t\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 本用例必须在参数校验阶段结束，不应访问存储。
			svc := NewKnowledgeService(nil)

			input := CreateKnowledgeDraftInput{
				DocumentKey: tt.documentKey,
				Version:     "v1",
				CategoryID:  1,
				Title:       "测试规则，非官方资料",
				Content:     "仅用于测试参数校验。",
				SourceURL:   "https://example.com/test-rule",
				Articles: []CreateRuleArticleInput{
					{
						Content: "仅用于测试参数校验。",
					},
				},
			}

			result, err := svc.CreateDraft(context.Background(), input)

			if !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("expected ErrInvalidArgument, got %v", err)
			}
			if result != nil {
				t.Fatalf("expected no created version, got %+v", result)
			}
		})
	}
}
