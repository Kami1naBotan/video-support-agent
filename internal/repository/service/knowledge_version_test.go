package service

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestKnowledgeServiceCreateDraftRejectsInvalidVersion(t *testing.T) {
	tests := []struct {
		name    string
		version string
	}{
		{
			name:    "empty",
			version: "",
		},
		{
			name:    "spaces_only",
			version: "   ",
		},
		{
			name:    "tabs_and_newlines_only",
			version: "\t\n",
		},
		{
			name:    "more_than_64_ascii_characters",
			version: strings.Repeat("v", 65),
		},
		{
			name:    "more_than_64_chinese_characters",
			version: strings.Repeat("版", 65),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := NewKnowledgeService(nil)

			input := CreateKnowledgeDraftInput{
				DocumentKey: "test-community-rule",
				Version:     tt.version,
				CategoryID:  1,
				Title:       "测试规则，非官方资料",
				Content:     "仅用于测试版本标识校验。",
				SourceURL:   "https://example.com/test-rule",
				Articles: []CreateRuleArticleInput{
					{
						SectionPath:   "第一章",
						ArticleNumber: "第一条",
						Content:       "仅用于测试版本标识校验。",
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
