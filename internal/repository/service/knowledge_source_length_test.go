package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestKnowledgeServiceCreateDraftRejectsTooLongSourceURL(t *testing.T) {
	const prefix = "https://example.com/"

	tests := []struct {
		name      string
		sourceURL string
	}{
		{
			name: "ascii_url_exceeds_2048_characters",
			sourceURL: prefix +
				strings.Repeat("a", 2049-utf8.RuneCountInString(prefix)),
		},
		{
			name: "unicode_url_exceeds_2048_characters",
			sourceURL: prefix +
				strings.Repeat("规", 2049-utf8.RuneCountInString(prefix)),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 明确验证测试输入的长度，避免构造错误导致误判。
			if got := utf8.RuneCountInString(tt.sourceURL); got != 2049 {
				t.Fatalf("test URL length = %d, want 2049", got)
			}

			svc := NewKnowledgeService(nil)

			input := CreateKnowledgeDraftInput{
				DocumentKey: "test-community-rule",
				Version:     "v1",
				CategoryID:  1,
				Title:       "测试规则，非官方资料",
				Content:     "仅用于测试来源地址长度。",
				SourceURL:   tt.sourceURL,
				Articles: []CreateRuleArticleInput{
					{
						SectionPath:   "第一章",
						ArticleNumber: "第一条",
						Content:       "仅用于测试来源地址长度。",
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
