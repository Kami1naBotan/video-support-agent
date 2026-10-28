package service

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestKnowledgeServiceCreateDraftRejectsInvalidTitleOrContent(t *testing.T) {
	tests := []struct {
		name    string
		title   string
		content string
	}{
		{
			name:    "empty_title",
			title:   "",
			content: "仅用于测试，非官方规则。",
		},
		{
			name:    "whitespace_only_title",
			title:   " \t\n ",
			content: "仅用于测试，非官方规则。",
		},
		{
			name:    "title_exceeds_255_ascii_characters",
			title:   strings.Repeat("a", 256),
			content: "仅用于测试，非官方规则。",
		},
		{
			name:    "title_exceeds_255_chinese_characters",
			title:   strings.Repeat("规", 256),
			content: "仅用于测试，非官方规则。",
		},
		{
			name:    "empty_content",
			title:   "测试规则，非官方资料",
			content: "",
		},
		{
			name:    "whitespace_only_content",
			title:   "测试规则，非官方资料",
			content: " \t\r\n ",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := NewKnowledgeService(nil)

			input := CreateKnowledgeDraftInput{
				DocumentKey: "test-community-rule",
				Version:     "v1",
				CategoryID:  1,
				Title:       tt.title,
				Content:     tt.content,
				SourceURL:   "https://example.com/test-rule",
				Articles: []CreateRuleArticleInput{
					{
						SectionPath:   "第一章",
						ArticleNumber: "第一条",
						Content:       "仅用于测试，非官方规则。",
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
