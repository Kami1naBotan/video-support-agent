package service

import (
	"context"
	"errors"
	"testing"
)

func TestKnowledgeServiceCreateDraftRejectsZeroCategoryID(t *testing.T) {
	svc := NewKnowledgeService(nil)

	input := CreateKnowledgeDraftInput{
		DocumentKey: "test-community-rule",
		Version:     "v1",
		CategoryID:  0,
		Title:       "测试规则，非官方资料",
		Content:     "仅用于测试分类参数校验。",
		SourceURL:   "https://example.com/test-rule",
		Articles: []CreateRuleArticleInput{
			{
				SectionPath:   "第一章",
				ArticleNumber: "第一条",
				Content:       "仅用于测试分类参数校验。",
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
}
