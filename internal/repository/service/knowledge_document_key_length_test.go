package service

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestKnowledgeServiceCreateDraftRejectsTooLongDocumentKey(t *testing.T) {
	tests := []struct {
		name        string
		documentKey string
	}{
		{
			name:        "ascii_key_exceeds_100_characters",
			documentKey: strings.Repeat("a", 101),
		},
		{
			name:        "unicode_key_exceeds_100_characters",
			documentKey: strings.Repeat("规", 101),
		},
		{
			name:        "key_still_exceeds_limit_after_trimming",
			documentKey: " \t" + strings.Repeat("a", 101) + "\n ",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := NewKnowledgeService(nil)

			input := CreateKnowledgeDraftInput{
				DocumentKey: tt.documentKey,
				Version:     "v1",
				CategoryID:  1,
				Title:       "测试规则，非官方资料",
				Content:     "仅用于测试文档标识长度。",
				SourceURL:   "https://example.com/test-rule",
				Articles: []CreateRuleArticleInput{
					{
						SectionPath:   "第一章",
						ArticleNumber: "第一条",
						Content:       "仅用于测试文档标识长度。",
					},
				},
			}

			result, err := svc.CreateDraft(context.Background(), input)

			if !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf(
					"CreateDraft() error = %v, want ErrInvalidArgument",
					err,
				)
			}

			if result != nil {
				t.Fatalf(
					"CreateDraft() result = %+v, want nil",
					result,
				)
			}
		})
	}
}
