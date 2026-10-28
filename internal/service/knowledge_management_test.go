package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"video-support-agent/internal/model"
	"video-support-agent/internal/service"
)

// 仅替代数据库边界，不执行 SQL。
// 意外调用未配置的方法时直接失败，不伪造成功。
type managementStore struct {
	edit func(context.Context, uint64, uint64, model.RuleVersion,
		[]model.RuleArticle) (*model.RuleVersion, *model.RuleVerification, error)
	verify func(context.Context, uint64,
		service.VerifyKnowledgeDraftInput) (*model.RuleVerification, error)
	detail func(context.Context, uint64) (
		*model.RuleVersion, []model.RuleArticle, *model.RuleVerification, error)
}

func (s *managementStore) CreateDraft(
	context.Context, *model.KnowledgeDocument,
	*model.RuleVersion, []model.RuleArticle,
) error {
	panic("unexpected CreateDraft")
}

func (s *managementStore) GetDraftByID(
	context.Context, uint64,
) (*model.RuleVersion, []model.RuleArticle, error) {
	panic("unexpected legacy GetDraftByID")
}

func (s *managementStore) EditDraftWithVerification(
	ctx context.Context, id, revision uint64,
	content model.RuleVersion, articles []model.RuleArticle,
) (*model.RuleVersion, *model.RuleVerification, error) {
	if s.edit == nil {
		panic("unexpected edit: invalid input reached storage")
	}
	return s.edit(ctx, id, revision, content, articles)
}

func (s *managementStore) VerifyDraft(
	ctx context.Context, userID uint64,
	input service.VerifyKnowledgeDraftInput,
) (*model.RuleVerification, error) {
	if s.verify == nil {
		panic("unexpected verification: invalid input reached storage")
	}
	return s.verify(ctx, userID, input)
}

func (s *managementStore) GetDraftDetailByID(
	ctx context.Context, id uint64,
) (*model.RuleVersion, []model.RuleArticle, *model.RuleVerification, error) {
	if s.detail == nil {
		panic("unexpected detail read")
	}
	return s.detail(ctx, id)
}

func managementEditInput() service.EditKnowledgeDraftInput {
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	return service.EditKnowledgeDraftInput{
		VersionID:               21,
		ExpectedContentRevision: 3,
		CategoryID:              7,
		Title:                   "  虚构测试标题  ",
		Content:                 "  虚构全文\r\n第二行\n",
		SourceURL:               "  https://www.bilibili.com/Test?key=AbC  ",
		EffectiveAt:             &start,
		ExpiresAt:               &end,
		Articles: []service.CreateRuleArticleInput{
			{
				SectionPath:   "  测试章节  ",
				ArticleNumber: "  第一条  ",
				Content:       "  虚构条款\r\n原格式\n",
			},
			{Content: "未编号的虚构条款"},
		},
	}
}

func managementJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestKnowledgeManagementEditNormalization(t *testing.T) {
	input := managementEditInput()
	before := managementJSON(t, input)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	saved := &model.RuleVersion{ID: 21, ContentRevision: 4}
	oldVerification := &model.RuleVerification{
		ID: 31, VersionID: 21, VerifiedContentRevision: 3,
	}
	calls := 0
	store := &managementStore{
		edit: func(gotCtx context.Context, id, revision uint64,
			content model.RuleVersion, articles []model.RuleArticle,
		) (*model.RuleVersion, *model.RuleVerification, error) {
			calls++
			if gotCtx != ctx || id != 21 || revision != 3 {
				t.Fatalf("wrong context or identity: id=%d revision=%d", id, revision)
			}
			if content.CategoryID != 7 ||
				content.Title != "虚构测试标题" ||
				content.Content != "  虚构全文\r\n第二行\n" ||
				content.SourceURL != "https://www.bilibili.com/Test?key=AbC" {
				t.Fatalf("wrong normalized content: %+v", content)
			}
			want := []model.RuleArticle{
				{
					ArticleOrder: 1, SectionPath: "测试章节",
					ArticleNumber: "第一条", Content: "  虚构条款\r\n原格式\n",
				},
				{ArticleOrder: 2, Content: "未编号的虚构条款"},
			}
			if !reflect.DeepEqual(articles, want) {
				t.Fatalf("articles=%+v, want=%+v", articles, want)
			}
			if content.EffectiveAt == input.EffectiveAt ||
				content.ExpiresAt == input.ExpiresAt {
				t.Fatal("date pointers still alias caller input")
			}
			if !content.EffectiveAt.Equal(*input.EffectiveAt) ||
				!content.ExpiresAt.Equal(*input.ExpiresAt) {
				t.Fatal("date values changed")
			}

			// 模拟存储边界修改收到的数据，调用方输入仍须保持不变。
			*content.EffectiveAt = content.EffectiveAt.Add(time.Hour)
			*content.ExpiresAt = content.ExpiresAt.Add(time.Hour)
			articles[0].Content = "boundary mutation"
			return saved, oldVerification, nil
		},
	}

	result, err := service.NewKnowledgeService(store).
		EditDraftWithVerification(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || result.Version != saved ||
		result.Verification != oldVerification || calls != 1 {
		t.Fatalf("wrong edit result or calls: result=%+v calls=%d", result, calls)
	}
	if managementJSON(t, input) != before {
		t.Fatal("editing modified caller input")
	}
}

func TestKnowledgeManagementEditRejectsInvalidInput(t *testing.T) {
	cases := []struct {
		name   string
		change func(*service.EditKnowledgeDraftInput)
	}{
		{"zero_version", func(v *service.EditKnowledgeDraftInput) { v.VersionID = 0 }},
		{"zero_revision", func(v *service.EditKnowledgeDraftInput) { v.ExpectedContentRevision = 0 }},
		{"zero_category", func(v *service.EditKnowledgeDraftInput) { v.CategoryID = 0 }},
		{"blank_title", func(v *service.EditKnowledgeDraftInput) { v.Title = " \n " }},
		{"long_title", func(v *service.EditKnowledgeDraftInput) { v.Title = strings.Repeat("规", 256) }},
		{"blank_content", func(v *service.EditKnowledgeDraftInput) { v.Content = "\t " }},
		{"empty_articles", func(v *service.EditKnowledgeDraftInput) { v.Articles = []service.CreateRuleArticleInput{} }},
		{"blank_article", func(v *service.EditKnowledgeDraftInput) { v.Articles[0].Content = " " }},
		{"long_section", func(v *service.EditKnowledgeDraftInput) { v.Articles[0].SectionPath = strings.Repeat("章", 501) }},
		{"long_number", func(v *service.EditKnowledgeDraftInput) { v.Articles[0].ArticleNumber = strings.Repeat("条", 65) }},
		{"equal_dates", func(v *service.EditKnowledgeDraftInput) {
			end := *v.EffectiveAt
			v.ExpiresAt = &end
		}},
		{"reversed_dates", func(v *service.EditKnowledgeDraftInput) {
			end := v.EffectiveAt.Add(-time.Second)
			v.ExpiresAt = &end
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := managementEditInput()
			tc.change(&input)
			before := managementJSON(t, input)
			result, err := service.NewKnowledgeService(&managementStore{}).
				EditDraftWithVerification(context.Background(), input)
			if result != nil || !errors.Is(err, service.ErrInvalidArgument) {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if managementJSON(t, input) != before {
				t.Fatal("rejected input was modified")
			}
		})
	}
}

func TestKnowledgeManagementEditAcceptsBoundaries(t *testing.T) {
	input := managementEditInput()
	input.Title = strings.Repeat("规", 255)
	input.Articles[0].SectionPath = strings.Repeat("章", 500)
	input.Articles[0].ArticleNumber = strings.Repeat("条", 64)
	input.SourceURL = "https://www.bilibili.com/" +
		strings.Repeat("a", 2048-len("https://www.bilibili.com/"))
	input.EffectiveAt, input.ExpiresAt = nil, nil

	called := false
	store := &managementStore{
		edit: func(_ context.Context, _, _ uint64,
			content model.RuleVersion, _ []model.RuleArticle,
		) (*model.RuleVersion, *model.RuleVerification, error) {
			called = true
			if content.EffectiveAt != nil || content.ExpiresAt != nil {
				t.Fatal("unknown dates were invented")
			}
			return &model.RuleVersion{ID: 21, ContentRevision: 4}, nil, nil
		},
	}
	result, err := service.NewKnowledgeService(store).
		EditDraftWithVerification(context.Background(), input)
	if err != nil || result == nil || !called {
		t.Fatalf("boundary rejected: result=%+v err=%v", result, err)
	}
}

func TestKnowledgeManagementVerificationNormalization(t *testing.T) {
	for _, evidence := range []string{
		"  https://www.bilibili.com/Test?key=AbC  ",
		" \t ",
	} {
		t.Run(fmt.Sprintf("evidence_%d", len(evidence)), func(t *testing.T) {
			input := service.VerifyKnowledgeDraftInput{
				VersionID: 21, ExpectedContentRevision: 3,
				VerificationNote: "  " + strings.Repeat("核", 2000) + "  ",
				EvidenceURL:      &evidence,
			}
			before := managementJSON(t, input)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			saved := &model.RuleVerification{
				ID: 31, VersionID: 21, VerifiedContentRevision: 3, VerifiedBy: 9,
			}
			calls := 0
			store := &managementStore{
				verify: func(gotCtx context.Context, userID uint64,
					got service.VerifyKnowledgeDraftInput,
				) (*model.RuleVerification, error) {
					calls++
					if gotCtx != ctx || userID != 9 ||
						got.VersionID != 21 || got.ExpectedContentRevision != 3 {
						t.Fatal("context, verifier or revision was not preserved")
					}
					if got.VerificationNote != strings.Repeat("核", 2000) {
						t.Fatal("note normalization or length boundary is wrong")
					}
					if strings.TrimSpace(evidence) == "" {
						if got.EvidenceURL != nil {
							t.Fatal("blank evidence must become nil")
						}
					} else {
						if got.EvidenceURL == nil ||
							*got.EvidenceURL != "https://www.bilibili.com/Test?key=AbC" {
							t.Fatal("evidence URL changed or was lost")
						}
						if got.EvidenceURL == input.EvidenceURL {
							t.Fatal("evidence pointer aliases caller input")
						}
						*got.EvidenceURL = "boundary mutation"
					}
					return saved, nil
				},
			}
			got, err := service.NewKnowledgeService(store).VerifyDraft(ctx, 9, input)
			if err != nil || got != saved || calls != 1 {
				t.Fatalf("got=%+v err=%v calls=%d", got, err, calls)
			}
			if managementJSON(t, input) != before {
				t.Fatal("verification modified caller input")
			}
		})
	}
}

func TestKnowledgeManagementVerificationRejectsInvalidInput(t *testing.T) {
	badURL := "https://www.bilibili.com.attacker.invalid/rule"
	cases := []struct {
		name  string
		user  uint64
		input service.VerifyKnowledgeDraftInput
		want  error
	}{
		{"missing_identity", 0, service.VerifyKnowledgeDraftInput{
			VersionID: 21, ExpectedContentRevision: 3, VerificationNote: "测试",
		}, service.ErrUnauthorized},
		{"missing_version", 9, service.VerifyKnowledgeDraftInput{
			ExpectedContentRevision: 3, VerificationNote: "测试",
		}, service.ErrInvalidArgument},
		{"missing_revision", 9, service.VerifyKnowledgeDraftInput{
			VersionID: 21, VerificationNote: "测试",
		}, service.ErrInvalidArgument},
		{"blank_note", 9, service.VerifyKnowledgeDraftInput{
			VersionID: 21, ExpectedContentRevision: 3, VerificationNote: " \n ",
		}, service.ErrInvalidArgument},
		{"long_note", 9, service.VerifyKnowledgeDraftInput{
			VersionID: 21, ExpectedContentRevision: 3,
			VerificationNote: strings.Repeat("核", 2001),
		}, service.ErrInvalidArgument},
		{"untrusted_evidence", 9, service.VerifyKnowledgeDraftInput{
			VersionID: 21, ExpectedContentRevision: 3,
			VerificationNote: "测试", EvidenceURL: &badURL,
		}, service.ErrInvalidArgument},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := managementJSON(t, tc.input)
			got, err := service.NewKnowledgeService(&managementStore{}).
				VerifyDraft(context.Background(), tc.user, tc.input)
			if got != nil || !errors.Is(err, tc.want) {
				t.Fatalf("got=%+v err=%v want=%v", got, err, tc.want)
			}
			if managementJSON(t, tc.input) != before {
				t.Fatal("rejected verification modified input")
			}
		})
	}
}

func TestKnowledgeManagementOfficialSourcePolicy(t *testing.T) {
	base := "https://www.bilibili.com/"
	valid := []string{
		base,
		" https://WWW.BILIBILI.COM/Test?key=AbC ",
		"https://www.bilibili.com:443/rule",
		"http://www.bilibili.com:80/rule",
		base + strings.Repeat("a", 2048-len(base)),
	}
	for _, raw := range valid {
		if err := service.ValidateKnowledgeOfficialSourceURL(raw); err != nil {
			t.Fatalf("valid URL of length %d rejected: %v", len(raw), err)
		}
	}
	invalid := []string{
		"", "   ",
		"https://example.invalid/rule",
		"https://www.bilibili.com.attacker.invalid/rule",
		"https://evilwww.bilibili.com/rule",
		"https://www.bilibili.com@attacker.invalid/rule",
		"https://user:pass@www.bilibili.com/rule",
		"https://www.bilibili.com:/rule",
		"https://www.bilibili.com:80/rule",
		"http://www.bilibili.com:443/rule",
		"ftp://www.bilibili.com/rule",
		"https://www.bilibili.com/%zz",
		base + strings.Repeat("a", 2049-len(base)),
	}
	for i, raw := range invalid {
		if err := service.ValidateKnowledgeOfficialSourceURL(raw); !errors.Is(err, service.ErrInvalidArgument) {
			t.Fatalf("invalid case %d: err=%v", i, err)
		}
	}
}

func TestKnowledgeManagementPreservesErrors(t *testing.T) {
	for _, sentinel := range []error{
		service.ErrKnowledgeVersionNotFound,
		service.ErrKnowledgeVersionNotEditable,
		service.ErrKnowledgeContentConflict,
		context.Canceled,
	} {
		t.Run(sentinel.Error(), func(t *testing.T) {
			wrapped := fmt.Errorf("storage: %w", sentinel)
			store := &managementStore{
				edit: func(context.Context, uint64, uint64, model.RuleVersion,
					[]model.RuleArticle,
				) (*model.RuleVersion, *model.RuleVerification, error) {
					return nil, nil, wrapped
				},
				verify: func(context.Context, uint64,
					service.VerifyKnowledgeDraftInput,
				) (*model.RuleVerification, error) {
					return nil, wrapped
				},
			}
			svc := service.NewKnowledgeService(store)
			edited, err := svc.EditDraftWithVerification(
				context.Background(), managementEditInput(),
			)
			if edited != nil || !errors.Is(err, sentinel) {
				t.Fatalf("edit lost error identity: %v", err)
			}
			verified, err := svc.VerifyDraft(
				context.Background(), 9,
				service.VerifyKnowledgeDraftInput{
					VersionID: 21, ExpectedContentRevision: 3,
					VerificationNote: "虚构测试",
				},
			)
			if verified != nil || !errors.Is(err, sentinel) {
				t.Fatalf("verification lost error identity: %v", err)
			}
		})
	}
}

func TestKnowledgeManagementDetailPreservesOldVerification(t *testing.T) {
	version := &model.RuleVersion{ID: 21, ContentRevision: 4}
	verification := &model.RuleVerification{
		ID: 31, VersionID: 21, VerifiedContentRevision: 3,
	}
	articles := []model.RuleArticle{
		{ID: 41, VersionID: 21, ArticleOrder: 1, Content: "虚构条款"},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store := &managementStore{
		detail: func(gotCtx context.Context, id uint64) (
			*model.RuleVersion, []model.RuleArticle, *model.RuleVerification, error,
		) {
			if gotCtx != ctx || id != 21 {
				t.Fatal("detail context or version ID changed")
			}
			return version, articles, verification, nil
		},
	}
	got, err := service.NewKnowledgeService(store).GetDraftDetailByID(ctx, 21)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Version != version ||
		got.Verification != verification || !reflect.DeepEqual(got.Articles, articles) {
		t.Fatalf("detail lost content or historical verification: %+v", got)
	}
}
