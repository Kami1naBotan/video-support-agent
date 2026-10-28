package service

import (
	"context"
	"errors"
	"testing"
)

func TestKnowledgeServiceCreateDraftRejectsInvalidSourceURL(t *testing.T) {
	tests := []struct {
		name      string
		sourceURL string
	}{
		{
			name:      "empty",
			sourceURL: "",
		},
		{
			name:      "whitespace_only",
			sourceURL: " \t\n ",
		},
		{
			name:      "relative_path",
			sourceURL: "/rules/community",
		},
		{
			name:      "missing_scheme",
			sourceURL: "example.com/rules",
		},
		{
			name:      "missing_host",
			sourceURL: "https:///rules",
		},
		{
			name:      "unsupported_scheme",
			sourceURL: "ftp://example.com/rules",
		},
		{
			name:      "invalid_escape",
			sourceURL: "https://example.com/%zz",
		},
		{
			name:      "contains_credentials",
			sourceURL: "https://user:password@example.com/rules",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := NewKnowledgeService(nil)

			input := CreateKnowledgeDraftInput{
				DocumentKey: "test-community-rule",
				Version:     "v1",
				CategoryID:  1,
				Title:       "测试规则，非官方资料",
				Content:     "仅用于测试来源地址校验。",
				SourceURL:   tt.sourceURL,
				Articles: []CreateRuleArticleInput{
					{
						SectionPath:   "第一章",
						ArticleNumber: "第一条",
						Content:       "仅用于测试来源地址校验。",
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
