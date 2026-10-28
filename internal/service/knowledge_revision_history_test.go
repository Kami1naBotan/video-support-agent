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

type revisionHistoryStore struct {
	managementStore

	create  func(context.Context, *model.RuleVersion, []model.RuleArticle) error
	history func(context.Context, uint64) (*service.KnowledgeHistoryDetail, error)
	article func(context.Context, uint64, uint64) (
		*service.KnowledgeHistoryArticleDetail, error)
}

func (s *revisionHistoryStore) CreateRevision(
	ctx context.Context, v *model.RuleVersion, articles []model.RuleArticle,
) error {
	if s.create == nil {
		panic("unexpected revision storage call")
	}
	return s.create(ctx, v, articles)
}

func (s *revisionHistoryStore) GetHistoryByVersionID(
	ctx context.Context, id uint64,
) (*service.KnowledgeHistoryDetail, error) {
	if s.history == nil {
		panic("unexpected history storage call")
	}
	return s.history(ctx, id)
}

func (s *revisionHistoryStore) GetHistoryArticle(
	ctx context.Context, versionID, articleID uint64,
) (*service.KnowledgeHistoryArticleDetail, error) {
	if s.article == nil {
		panic("unexpected history article storage call")
	}
	return s.article(ctx, versionID, articleID)
}

func revisionHistoryInput() service.CreateKnowledgeRevisionInput {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(1, 0, 0)
	url := "  https://www.bilibili.com/test-only  "
	return service.CreateKnowledgeRevisionInput{
		DocumentID:        11,
		Version:           "  test-v2  ",
		RevisionType:      model.RuleRevisionTypeOfficial,
		RevisionNote:      "  【虚构测试】修订依据  ",
		RevisionSourceURL: &url,
		CategoryID:        7,
		Title:             "  【虚构测试】标题  ",
		Content:           "  【虚构测试】正文\r\n保留格式\n",
		SourceURL:         "  https://www.bilibili.com/test-only  ",
		EffectiveAt:       &start,
		ExpiresAt:         &end,
		Articles: []service.CreateRuleArticleInput{
			{
				SectionPath:   "  虚构章节  ",
				ArticleNumber: "  一  ",
				Content:       "  【虚构测试】条款\r\n",
			},
			{Content: "【虚构测试】无编号条款"},
		},
	}
}

func revisionHistoryJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestKnowledgeRevisionServiceRejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name string
		edit func(*service.CreateKnowledgeRevisionInput)
	}{
		{"zero_document", func(i *service.CreateKnowledgeRevisionInput) {
			i.DocumentID = 0
		}},
		{"blank_version", func(i *service.CreateKnowledgeRevisionInput) {
			i.Version = " \t"
		}},
		{"long_version", func(i *service.CreateKnowledgeRevisionInput) {
			i.Version = strings.Repeat("版", 65)
		}},
		{"missing_type", func(i *service.CreateKnowledgeRevisionInput) {
			i.RevisionType = ""
		}},
		{"unknown_type", func(i *service.CreateKnowledgeRevisionInput) {
			i.RevisionType = model.RuleRevisionTypeUnknown
		}},
		{"unsupported_type", func(i *service.CreateKnowledgeRevisionInput) {
			i.RevisionType = "OFFICIAL"
		}},
		{"blank_note", func(i *service.CreateKnowledgeRevisionInput) {
			i.RevisionNote = " \n"
		}},
		{"long_note", func(i *service.CreateKnowledgeRevisionInput) {
			i.RevisionNote = strings.Repeat("据", 2001)
		}},
		{"official_missing_url", func(i *service.CreateKnowledgeRevisionInput) {
			i.RevisionSourceURL = nil
		}},
		{"official_blank_url", func(i *service.CreateKnowledgeRevisionInput) {
			value := " "
			i.RevisionSourceURL = &value
		}},
		{"editorial_bad_url", func(i *service.CreateKnowledgeRevisionInput) {
			i.RevisionType = model.RuleRevisionTypeEditorial
			value := "https://www.bilibili.com.attacker.example/test"
			i.RevisionSourceURL = &value
		}},
		{"zero_category", func(i *service.CreateKnowledgeRevisionInput) {
			i.CategoryID = 0
		}},
		{"blank_title", func(i *service.CreateKnowledgeRevisionInput) {
			i.Title = " "
		}},
		{"blank_content", func(i *service.CreateKnowledgeRevisionInput) {
			i.Content = "\r\n"
		}},
		{"bad_source", func(i *service.CreateKnowledgeRevisionInput) {
			i.SourceURL = "file:///test"
		}},
		{"equal_dates", func(i *service.CreateKnowledgeRevisionInput) {
			value := *i.EffectiveAt
			i.ExpiresAt = &value
		}},
		{"no_articles", func(i *service.CreateKnowledgeRevisionInput) {
			i.Articles = nil
		}},
		{"blank_article", func(i *service.CreateKnowledgeRevisionInput) {
			i.Articles[0].Content = " "
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			input := revisionHistoryInput()
			tc.edit(&input)
			before := revisionHistoryJSON(t, input)

			svc := service.NewKnowledgeService(&revisionHistoryStore{})
			got, err := svc.CreateRevision(context.Background(), input)
			if got != nil || !errors.Is(err, service.ErrInvalidArgument) {
				t.Fatalf("result=%+v error=%v", got, err)
			}
			if revisionHistoryJSON(t, input) != before {
				t.Fatal("rejected request modified caller input")
			}
		})
	}
}

func TestKnowledgeRevisionServiceNormalizesWithoutMutatingInput(t *testing.T) {
	input := revisionHistoryInput()
	before := revisionHistoryJSON(t, input)
	type contextKey struct{}
	ctx := context.WithValue(context.Background(), contextKey{}, "request")
	calls := 0

	store := &revisionHistoryStore{
		create: func(gotCtx context.Context, v *model.RuleVersion, a []model.RuleArticle) error {
			calls++
			if gotCtx != ctx {
				t.Fatal("request context was not forwarded")
			}
			if v.ID != 0 || v.DocumentID != 11 || v.Version != "test-v2" ||
				v.CategoryID != 7 || v.ContentRevision != 1 ||
				v.Status != model.RuleVersionStatusDraft ||
				v.PublishedAt != nil || v.WithdrawnAt != nil {
				t.Fatalf("incorrect new draft identity/state: %+v", v)
			}
			if v.RevisionType != model.RuleRevisionTypeOfficial ||
				v.RevisionNote != "【虚构测试】修订依据" ||
				v.RevisionSourceURL == nil ||
				*v.RevisionSourceURL != "https://www.bilibili.com/test-only" {
				t.Fatalf("incorrect revision metadata: %+v", v)
			}
			if v.Title != "【虚构测试】标题" ||
				v.SourceURL != "https://www.bilibili.com/test-only" ||
				v.Content != input.Content ||
				!v.EffectiveAt.Equal(*input.EffectiveAt) ||
				!v.ExpiresAt.Equal(*input.ExpiresAt) {
				t.Fatalf("incorrect normalized content: %+v", v)
			}
			want := []model.RuleArticle{
				{
					ArticleOrder: 1, SectionPath: "虚构章节",
					ArticleNumber: "一", Content: input.Articles[0].Content,
				},
				{ArticleOrder: 2, Content: input.Articles[1].Content},
			}
			if !reflect.DeepEqual(a, want) {
				t.Fatalf("articles=%+v want=%+v", a, want)
			}

			// 主动修改传给仓储的副本，检测是否与调用方共享可变数据。
			*v.RevisionSourceURL = "https://www.bilibili.com/changed"
			*v.EffectiveAt = v.EffectiveAt.Add(time.Hour)
			*v.ExpiresAt = v.ExpiresAt.Add(time.Hour)
			a[0].Content = "storage-side mutation"
			v.ID = 21
			return nil
		},
	}

	got, err := service.NewKnowledgeService(store).CreateRevision(ctx, input)
	if err != nil || got == nil || got.ID != 21 || calls != 1 {
		t.Fatalf("result=%+v calls=%d error=%v", got, calls, err)
	}
	if revisionHistoryJSON(t, input) != before {
		t.Fatal("storage-side mutation escaped into caller input")
	}
}

func TestKnowledgeRevisionServiceAcceptsBoundariesAndEditorial(t *testing.T) {
	for _, kind := range []model.RuleRevisionType{
		model.RuleRevisionTypeOfficial,
		model.RuleRevisionTypeEditorial,
	} {
		t.Run(string(kind), func(t *testing.T) {
			input := revisionHistoryInput()
			input.RevisionType = kind
			input.Version = strings.Repeat("版", 64)
			input.RevisionNote = strings.Repeat("据", 2000)
			base := "https://www.bilibili.com/"
			url := base + strings.Repeat("a", 2048-len(base))
			input.RevisionSourceURL = &url
			if kind == model.RuleRevisionTypeEditorial {
				input.RevisionSourceURL = nil
			}
			called := false
			store := &revisionHistoryStore{
				create: func(_ context.Context, v *model.RuleVersion, _ []model.RuleArticle) error {
					called = true
					if v.Version != input.Version ||
						v.RevisionNote != input.RevisionNote ||
						!reflect.DeepEqual(v.RevisionSourceURL, input.RevisionSourceURL) {
						t.Fatalf("boundary input changed unexpectedly: %+v", v)
					}
					return nil
				},
			}
			got, err := service.NewKnowledgeService(store).
				CreateRevision(context.Background(), input)
			if err != nil || got == nil || !called {
				t.Fatalf("called=%v result=%+v error=%v", called, got, err)
			}
		})
	}
}

func TestKnowledgeRevisionServicePreservesStorageErrors(t *testing.T) {
	for _, want := range []error{
		service.ErrKnowledgeDocumentNotFound,
		service.ErrRuleCategoryNotFound,
		service.ErrKnowledgeVersionExists,
		context.Canceled,
		errors.New("controlled storage failure"),
	} {
		t.Run(want.Error(), func(t *testing.T) {
			store := &revisionHistoryStore{
				create: func(context.Context, *model.RuleVersion, []model.RuleArticle) error {
					return fmt.Errorf("storage: %w", want)
				},
			}
			got, err := service.NewKnowledgeService(store).
				CreateRevision(context.Background(), revisionHistoryInput())
			if got != nil || !errors.Is(err, want) {
				t.Fatalf("result=%+v error=%v want=%v", got, err, want)
			}
		})
	}
}

func TestKnowledgeHistoryServiceRejectsZeroIDs(t *testing.T) {
	svc := service.NewKnowledgeService(&revisionHistoryStore{})
	got, err := svc.GetHistoryByVersionID(context.Background(), 0)
	if got != nil || !errors.Is(err, service.ErrInvalidArgument) {
		t.Fatalf("result=%+v error=%v", got, err)
	}
	for _, ids := range [][2]uint64{{0, 91}, {21, 0}, {0, 0}} {
		got, err := svc.GetHistoryArticle(context.Background(), ids[0], ids[1])
		if got != nil || !errors.Is(err, service.ErrInvalidArgument) {
			t.Fatalf("ids=%v result=%+v error=%v", ids, got, err)
		}
	}
}

func TestKnowledgeHistoryServiceVisibilityAndOwnership(t *testing.T) {
	tests := []struct {
		name string
		edit func(*model.RuleVersion, *model.RuleArticle)
		want error
	}{
		{"published", func(*model.RuleVersion, *model.RuleArticle) {}, nil},
		{"withdrawn", func(v *model.RuleVersion, _ *model.RuleArticle) {
			v.Status = model.RuleVersionStatusWithdrawn
		}, nil},
		{"draft_even_with_publication_date", func(v *model.RuleVersion, _ *model.RuleArticle) {
			v.Status = model.RuleVersionStatusDraft
		}, service.ErrKnowledgeVersionNotFound},
		{"never_published", func(v *model.RuleVersion, _ *model.RuleArticle) {
			v.PublishedAt = nil
		}, service.ErrKnowledgeVersionNotFound},
		{"wrong_version", func(v *model.RuleVersion, _ *model.RuleArticle) {
			v.ID = 22
		}, service.ErrKnowledgeVersionNotFound},
		{"wrong_article_id", func(_ *model.RuleVersion, a *model.RuleArticle) {
			a.ID = 92
		}, service.ErrKnowledgeArticleNotFound},
		{"wrong_article_owner", func(_ *model.RuleVersion, a *model.RuleArticle) {
			a.VersionID = 22
		}, service.ErrKnowledgeArticleNotFound},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			v := model.RuleVersion{
				ID: 21, DocumentID: 11, Status: model.RuleVersionStatusPublished,
				PublishedAt: &at, SourceURL: "https://www.bilibili.com/test-only",
			}
			a := model.RuleArticle{ID: 91, VersionID: 21, Content: "【虚构测试】"}
			tc.edit(&v, &a)
			state := service.KnowledgeHistoryState{
				IsSuperseded: true, IsExpired: true,
			}
			ctx := context.Background()
			store := &revisionHistoryStore{
				history: func(c context.Context, id uint64) (*service.KnowledgeHistoryDetail, error) {
					if c != ctx || id != 21 {
						t.Fatal("history arguments not forwarded")
					}
					return &service.KnowledgeHistoryDetail{
						Version: &v, Articles: []model.RuleArticle{a}, State: state,
					}, nil
				},
				article: func(c context.Context, vid, aid uint64) (*service.KnowledgeHistoryArticleDetail, error) {
					if c != ctx || vid != 21 || aid != 91 {
						t.Fatal("article arguments not forwarded")
					}
					return &service.KnowledgeHistoryArticleDetail{
						Version: &v, Article: &a, State: state,
					}, nil
				},
			}
			svc := service.NewKnowledgeService(store)
			single, err := svc.GetHistoryArticle(ctx, 21, 91)
			if tc.want != nil {
				if single != nil || !errors.Is(err, tc.want) {
					t.Fatalf("single=%+v error=%v want=%v", single, err, tc.want)
				}
			} else if err != nil || single == nil ||
				single.State != state || single.Article.Content != a.Content {
				t.Fatalf("single=%+v error=%v", single, err)
			}

			all, err := svc.GetHistoryByVersionID(ctx, 21)
			switch {
			case errors.Is(tc.want, service.ErrKnowledgeVersionNotFound):
				if all != nil || !errors.Is(err, tc.want) {
					t.Fatalf("all=%+v error=%v", all, err)
				}
			case tc.name == "wrong_article_owner":
				if all != nil || err == nil {
					t.Fatal("whole history accepted an unrelated article")
				}
			default:
				// 整版本入口没有指定article_id，只要求条款属于该版本。
				if err != nil || all == nil || all.State != state ||
					!reflect.DeepEqual(all.Articles, []model.RuleArticle{a}) {
					t.Fatalf("all=%+v error=%v", all, err)
				}
			}
		})
	}
}

func TestKnowledgeHistoryServicePreservesStorageErrors(t *testing.T) {
	for _, want := range []error{
		service.ErrKnowledgeVersionNotFound,
		service.ErrKnowledgeArticleNotFound,
		context.Canceled,
		errors.New("controlled history failure"),
	} {
		t.Run(want.Error(), func(t *testing.T) {
			store := &revisionHistoryStore{
				history: func(context.Context, uint64) (*service.KnowledgeHistoryDetail, error) {
					return nil, fmt.Errorf("storage: %w", want)
				},
				article: func(context.Context, uint64, uint64) (*service.KnowledgeHistoryArticleDetail, error) {
					return nil, fmt.Errorf("storage: %w", want)
				},
			}
			svc := service.NewKnowledgeService(store)
			all, err := svc.GetHistoryByVersionID(context.Background(), 21)
			if all != nil || !errors.Is(err, want) {
				t.Fatalf("all=%+v error=%v want=%v", all, err, want)
			}
			single, err := svc.GetHistoryArticle(context.Background(), 21, 91)
			if single != nil || !errors.Is(err, want) {
				t.Fatalf("single=%+v error=%v want=%v", single, err, want)
			}
		})
	}
}
