package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"video-support-agent/internal/handler"
	"video-support-agent/internal/middleware"
	"video-support-agent/internal/model"
	"video-support-agent/internal/service"

	"github.com/gin-gonic/gin"
)

type ContextKey struct{}

type Store struct {
	create func(context.Context, *model.KnowledgeDocument,
		*model.RuleVersion, []model.RuleArticle) error
	get func(context.Context, uint64) (
		*model.RuleVersion, []model.RuleArticle, error)
}

func (s Store) CreateDraft(
	ctx context.Context,
	d *model.KnowledgeDocument,
	v *model.RuleVersion,
	a []model.RuleArticle,
) error {
	return s.create(ctx, d, v, a)
}

func (s Store) GetDraftByID(
	ctx context.Context, id uint64,
) (*model.RuleVersion, []model.RuleArticle, error) {
	return s.get(ctx, id)
}

type Business struct {
	create func(context.Context, service.CreateKnowledgeDraftInput) (
		*model.RuleVersion, error)
	get func(context.Context, uint64) (*service.KnowledgeDraftDetail, error)
}

func (s Business) CreateDraft(
	ctx context.Context, in service.CreateKnowledgeDraftInput,
) (*model.RuleVersion, error) {
	return s.create(ctx, in)
}

func (s Business) GetDraftByID(
	ctx context.Context, id uint64,
) (*service.KnowledgeDraftDetail, error) {
	return s.get(ctx, id)
}

func (s Business) GetDraftDetailByID(
	ctx context.Context,
	id uint64,
) (*service.KnowledgeDraftDetail, error) {
	return s.get(ctx, id)
}

func (s Business) EditDraftWithVerification(
	context.Context,
	service.EditKnowledgeDraftInput,
) (*service.KnowledgeDraftEditResult, error) {
	panic("unexpected edit in create/read contract tests")
}

func (s Business) VerifyDraft(
	context.Context,
	uint64,
	service.VerifyKnowledgeDraftInput,
) (*model.RuleVerification, error) {
	panic("unexpected verification in create/read contract tests")
}

func Input() service.CreateKnowledgeDraftInput {
	return service.CreateKnowledgeDraftInput{
		DocumentKey: "test-fiction",
		Version:     "test-v1",
		CategoryID:  1,
		Title:       "虚构测试资料，不是官方规则",
		Content:     "  虚构全文\r\n第二行\n",
		SourceURL:   "https://example.invalid/test",
		Articles: []service.CreateRuleArticleInput{
			{
				SectionPath:   "测试章/测试节",
				ArticleNumber: "测试编号",
				Content:       "  虚构条款甲\r\n保留格式\n",
			},
			{
				SectionPath:   "",
				ArticleNumber: "",
				Content:       "虚构未编号条款乙",
			},
		},
	}
}

func copyKnowledgeInput(
	in service.CreateKnowledgeDraftInput,
) service.CreateKnowledgeDraftInput {
	out := in

	// 保留 nil 与非 nil 空切片的区别，同时独立复制非空切片。
	if in.Articles != nil {
		out.Articles = make([]service.CreateRuleArticleInput, len(in.Articles))
		copy(out.Articles, in.Articles)
	}

	if in.EffectiveAt != nil {
		value := *in.EffectiveAt
		out.EffectiveAt = &value
	}

	if in.ExpiresAt != nil {
		value := *in.ExpiresAt
		out.ExpiresAt = &value
	}

	return out
}
func Must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func Equal(t *testing.T, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v\nwant %#v", got, want)
	}
}

func JSON(t *testing.T, value any) string {
	t.Helper()
	b, err := json.Marshal(value)
	Must(t, err)
	return string(b)
}

// 这里显式构造 HTTP 请求字段，不能直接序列化服务输入结构体。
func Payload(in service.CreateKnowledgeDraftInput) map[string]any {
	articles := make([]map[string]any, len(in.Articles))
	for i, a := range in.Articles {
		articles[i] = map[string]any{
			"section_path":   a.SectionPath,
			"article_number": a.ArticleNumber,
			"content":        a.Content,
		}
	}
	return map[string]any{
		"document_key": in.DocumentKey,
		"version":      in.Version,
		"category_id":  in.CategoryID,
		"title":        in.Title,
		"content":      in.Content,
		"source_url":   in.SourceURL,
		"effective_at": in.EffectiveAt,
		"expires_at":   in.ExpiresAt,
		"articles":     articles,
	}
}

func Request(
	h http.Handler, method, path, body, token string, unknownLength bool,
) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if unknownLength {
		req.ContentLength = -1
	}
	req = req.WithContext(context.WithValue(
		req.Context(), ContextKey{}, "request-marker",
	))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func WantJSON(t *testing.T, w *httptest.ResponseRecorder, status int, want any) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("HTTP status=%d want=%d body=%s", w.Code, status, w.Body.String())
	}
	if !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("unexpected Content-Type: %q", w.Header().Get("Content-Type"))
	}
	var gotValue, wantValue any
	Must(t, json.Unmarshal(w.Body.Bytes(), &gotValue))
	Must(t, json.Unmarshal([]byte(JSON(t, want)), &wantValue))
	Equal(t, gotValue, wantValue)
}

func WantError(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("HTTP status=%d want=%d body=%s", w.Code, status, w.Body.String())
	}
	var body map[string]any
	Must(t, json.Unmarshal(w.Body.Bytes(), &body))
	if len(body) != 2 || body["code"] != code {
		t.Fatalf("unexpected error response: %s", w.Body.String())
	}
	if message, ok := body["message"].(string); !ok || message == "" {
		t.Fatalf("missing error message: %s", w.Body.String())
	}
}

func Handler(s handler.KnowledgeService) *gin.Engine {
	r := gin.New()
	r.Use(middleware.BodyLimit(64 * 1024))
	h := handler.NewKnowledgeHandler(s)
	r.POST("/documents", h.Create)
	r.GET("/versions/:version_id", h.GetByID)
	return r
}

func TestKnowledgeServiceBoundariesAndNormalization(t *testing.T) {
	for _, character := range []string{"a", "规"} {
		t.Run(character, func(t *testing.T) {
			in := Input()
			in.DocumentKey = " \t" + strings.Repeat(character, 100) + "\n"
			in.Version = " " + strings.Repeat(character, 64) + " "
			in.Title = "\t" + strings.Repeat(character, 255) + "\n"
			prefix := "https://example.invalid/"
			in.SourceURL = " " + prefix +
				strings.Repeat(character, 2048-len(prefix)) + " "
			in.Articles[0].SectionPath = " " + strings.Repeat(character, 500) + " "
			in.Articles[0].ArticleNumber = " " + strings.Repeat(character, 64) + " "
			in.Articles[1].SectionPath = " \t"
			in.Articles[1].ArticleNumber = "\n "
			before := copyKnowledgeInput(in)

			calls := 0
			ctx := context.WithValue(context.Background(), ContextKey{}, "service-marker")
			store := Store{
				create: func(
					gotCtx context.Context,
					d *model.KnowledgeDocument,
					v *model.RuleVersion,
					articles []model.RuleArticle,
				) error {
					calls++
					Equal(t, gotCtx.Value(ContextKey{}), "service-marker")
					Equal(t, d.DocumentKey, strings.TrimSpace(in.DocumentKey))
					Equal(t, d.CurrentVersionID, (*uint64)(nil))
					Equal(t, d.PublicationRevision, uint64(0))
					Equal(t, v.Status, model.RuleVersionStatusDraft)
					Equal(t, v.Version, strings.TrimSpace(in.Version))
					Equal(t, v.CategoryID, in.CategoryID)
					Equal(t, v.Title, strings.TrimSpace(in.Title))
					Equal(t, v.SourceURL, strings.TrimSpace(in.SourceURL))
					Equal(t, v.Content, in.Content)
					Equal(t, len(articles), 2)
					for i, a := range articles {
						Equal(t, a.ArticleOrder, uint32(i+1))
						Equal(t, a.SectionPath, strings.TrimSpace(in.Articles[i].SectionPath))
						Equal(t, a.ArticleNumber, strings.TrimSpace(in.Articles[i].ArticleNumber))
						Equal(t, a.Content, in.Articles[i].Content)
					}
					d.ID = 11
					v.ID, v.DocumentID = 22, d.ID
					return nil
				},
			}
			got, err := service.NewKnowledgeService(store).CreateDraft(ctx, in)
			Must(t, err)
			if got == nil || got.ID != 22 || got.DocumentID != 11 {
				t.Fatalf("unexpected create result: %+v", got)
			}
			Equal(t, calls, 1)
			Equal(t, in, before)
		})
	}
}

func TestKnowledgeServiceDatesAndArticles(t *testing.T) {
	start := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	earlier := start.Add(-time.Hour)

	cases := []struct {
		name string
		edit func(*service.CreateKnowledgeDraftInput)
		ok   bool
	}{
		{"both_dates_null", func(*service.CreateKnowledgeDraftInput) {}, true},
		{"future_start_only", func(in *service.CreateKnowledgeDraftInput) {
			in.EffectiveAt = &start
		}, true},
		{"end_only", func(in *service.CreateKnowledgeDraftInput) {
			in.ExpiresAt = &end
		}, true},
		{"future_interval", func(in *service.CreateKnowledgeDraftInput) {
			in.EffectiveAt, in.ExpiresAt = &start, &end
		}, true},
		{"equal_dates", func(in *service.CreateKnowledgeDraftInput) {
			in.EffectiveAt, in.ExpiresAt = &start, &start
		}, false},
		{"reversed_dates", func(in *service.CreateKnowledgeDraftInput) {
			in.EffectiveAt, in.ExpiresAt = &start, &earlier
		}, false},
		{"nil_articles", func(in *service.CreateKnowledgeDraftInput) {
			in.Articles = nil
		}, false},
		{"empty_articles", func(in *service.CreateKnowledgeDraftInput) {
			in.Articles = []service.CreateRuleArticleInput{}
		}, false},
		{"empty_article_content", func(in *service.CreateKnowledgeDraftInput) {
			in.Articles[1].Content = ""
		}, false},
		{"blank_article_content", func(in *service.CreateKnowledgeDraftInput) {
			in.Articles[1].Content = " \r\n\t"
		}, false},
		{"section_ascii_over", func(in *service.CreateKnowledgeDraftInput) {
			in.Articles[1].SectionPath = strings.Repeat("a", 501)
		}, false},
		{"section_unicode_over", func(in *service.CreateKnowledgeDraftInput) {
			in.Articles[1].SectionPath = strings.Repeat("章", 501)
		}, false},
		{"number_ascii_over", func(in *service.CreateKnowledgeDraftInput) {
			in.Articles[1].ArticleNumber = strings.Repeat("a", 65)
		}, false},
		{"number_unicode_over", func(in *service.CreateKnowledgeDraftInput) {
			in.Articles[1].ArticleNumber = strings.Repeat("条", 65)
		}, false},
		{"http_source_allowed", func(in *service.CreateKnowledgeDraftInput) {
			in.SourceURL = "http://example.invalid/test-only"
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := Input()
			tc.edit(&in)
			before := copyKnowledgeInput(in)
			calls := 0
			s := service.NewKnowledgeService(Store{
				create: func(
					_ context.Context, _ *model.KnowledgeDocument,
					v *model.RuleVersion, _ []model.RuleArticle,
				) error {
					calls++
					Equal(t, v.EffectiveAt, in.EffectiveAt)
					Equal(t, v.ExpiresAt, in.ExpiresAt)
					v.ID = 1
					return nil
				},
			})
			got, err := s.CreateDraft(context.Background(), in)
			if tc.ok {
				Must(t, err)
				if got == nil {
					t.Fatal("successful create returned nil")
				}
				Equal(t, calls, 1)
			} else {
				if !errors.Is(err, service.ErrInvalidArgument) || got != nil {
					t.Fatalf("got=(%+v,%v), want nil/ErrInvalidArgument", got, err)
				}
				Equal(t, calls, 0)
			}
			Equal(t, in, before)
		})
	}
}

func TestKnowledgeServiceErrorPropagation(t *testing.T) {
	for _, want := range []error{
		service.ErrRuleCategoryNotFound,
		service.ErrKnowledgeDocumentExists,
		errors.New("test storage failure"),
	} {
		t.Run(want.Error(), func(t *testing.T) {
			s := service.NewKnowledgeService(Store{
				create: func(
					context.Context, *model.KnowledgeDocument,
					*model.RuleVersion, []model.RuleArticle,
				) error {
					return fmt.Errorf("store: %w", want)
				},
			})
			got, err := s.CreateDraft(context.Background(), Input())
			if got != nil || !errors.Is(err, want) {
				t.Fatalf("got=(%+v,%v), want nil/wrapped original error", got, err)
			}
		})
	}

	calls := 0
	s := service.NewKnowledgeService(Store{
		get: func(ctx context.Context, id uint64) (
			*model.RuleVersion, []model.RuleArticle, error,
		) {
			calls++
			Equal(t, id, uint64(123))
			Equal(t, ctx.Value(ContextKey{}), "read-marker")
			return nil, nil, fmt.Errorf("store: %w", service.ErrKnowledgeDraftNotFound)
		},
	})
	got, err := s.GetDraftByID(context.Background(), 0)
	if got != nil || !errors.Is(err, service.ErrInvalidArgument) || calls != 0 {
		t.Fatalf("zero ID: result=%+v error=%v calls=%d", got, err, calls)
	}
	ctx := context.WithValue(context.Background(), ContextKey{}, "read-marker")
	got, err = s.GetDraftByID(ctx, 123)
	if got != nil || !errors.Is(err, service.ErrKnowledgeDraftNotFound) || calls != 1 {
		t.Fatalf("missing draft: result=%+v error=%v calls=%d", got, err, calls)
	}
}

func TestKnowledgeHTTPMapping(t *testing.T) {
	for _, withDate := range []bool{false, true} {
		t.Run(fmt.Sprintf("date_%v", withDate), func(t *testing.T) {
			in := Input()
			if withDate {
				date := time.Date(2099, 2, 3, 4, 5, 6, 0, time.UTC)
				in.EffectiveAt = &date
			}
			createCalls, getCalls := 0, 0
			r := Handler(Business{
				create: func(ctx context.Context, got service.CreateKnowledgeDraftInput) (
					*model.RuleVersion, error,
				) {
					createCalls++
					Equal(t, ctx.Value(ContextKey{}), "request-marker")
					Equal(t, got, in)
					return &model.RuleVersion{
						ID: 22, DocumentID: 11, Version: in.Version,
						Status:  model.RuleVersionStatusDraft,
						Content: "must not leak in create response",
						Document: model.KnowledgeDocument{
							PublicationRevision: 99,
						},
					}, nil
				},
				get: func(ctx context.Context, id uint64) (*service.KnowledgeDraftDetail, error) {
					getCalls++
					Equal(t, ctx.Value(ContextKey{}), "request-marker")
					Equal(t, id, uint64(22))
					return &service.KnowledgeDraftDetail{
						Version: &model.RuleVersion{
							ID: 22, DocumentID: 11, Version: in.Version,
							CategoryID: in.CategoryID, Title: in.Title,
							Content: in.Content, SourceURL: in.SourceURL,
							ContentRevision: 3,
							Status:          model.RuleVersionStatusDraft,
							EffectiveAt:     in.EffectiveAt,
							Document:        model.KnowledgeDocument{PublicationRevision: 99},
						},
						Articles: []model.RuleArticle{
							{
								ID: 32, VersionID: 22, ArticleOrder: 1,
								SectionPath:   in.Articles[0].SectionPath,
								ArticleNumber: in.Articles[0].ArticleNumber,
								Content:       in.Articles[0].Content,
							},
							{
								ID: 31, VersionID: 22, ArticleOrder: 2,
								Content: in.Articles[1].Content,
							},
						},
					}, nil
				},
			})
			w := Request(r, "POST", "/documents", JSON(t, Payload(in)), "", false)
			WantJSON(t, w, 201, map[string]any{
				"document_id": 11, "version_id": 22,
				"version": in.Version, "status": "draft",
			})
			w = Request(r, "GET", "/versions/22", "", "", false)
			WantJSON(t, w, 200, map[string]any{
				"document_id": 11, "version_id": 22,
				"version": in.Version, "category_id": in.CategoryID,
				"title": in.Title, "content": in.Content, "source_url": in.SourceURL,
				"status": "draft", "effective_at": in.EffectiveAt, "expires_at": nil,
				"articles": []map[string]any{
					{
						"article_id": 32, "article_order": 1,
						"section_path":   in.Articles[0].SectionPath,
						"article_number": in.Articles[0].ArticleNumber,
						"content":        in.Articles[0].Content,
					},
					{
						"article_id": 31, "article_order": 2,
						"section_path": "", "article_number": "",
						"content": in.Articles[1].Content,
					},
				},
				"content_revision":    3,
				"verification":        nil,
				"revision_type":       "",
				"revision_note":       "",
				"revision_source_url": nil,
			})
			Equal(t, createCalls, 1)
			Equal(t, getCalls, 1)
		})
	}
}

func TestKnowledgeHTTPRejectsMalformedInput(t *testing.T) {
	valid := JSON(t, Payload(Input()))
	bodies := map[string]string{
		"empty":         "",
		"null":          "null",
		"broken_json":   "{",
		"array":         "[]",
		"scalar":        `"text"`,
		"wrong_id_type": `{"category_id":"1"}`,
		"negative_id":   `{"category_id":-1}`,
		"overflow_id":   `{"category_id":18446744073709551616}`,
		"invalid_time":  `{"effective_at":"tomorrow"}`,
		"second_object": valid + `{}`,
		"second_null":   valid + `null`,
		"trailing_junk": valid + `x`,
	}
	for _, field := range []string{
		"status", "id", "document_id", "version_id",
		"publication_revision", "verified_by", "unexpected",
	} {
		payload := Payload(Input())
		payload[field] = 1
		bodies["unknown_"+field] = JSON(t, payload)
	}
	payload := Payload(Input())
	payload["articles"] = []map[string]any{
		{"content": "虚构条款", "article_id": 123},
	}
	bodies["unknown_article_field"] = JSON(t, payload)

	r := Handler(Business{
		create: func(context.Context, service.CreateKnowledgeDraftInput) (*model.RuleVersion, error) {
			t.Fatal("invalid JSON reached service")
			return nil, nil
		},
		get: func(context.Context, uint64) (*service.KnowledgeDraftDetail, error) {
			t.Fatal("invalid ID reached service")
			return nil, nil
		},
	})
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			w := Request(r, "POST", "/documents", body, "", false)
			WantError(t, w, 400, "invalid_argument")
		})
	}
	for _, id := range []string{"abc", "0", "-1", "18446744073709551616", "1.5"} {
		t.Run("id_"+id, func(t *testing.T) {
			w := Request(r, "GET", "/versions/"+id, "", "", false)
			WantError(t, w, 400, "invalid_argument")
		})
	}

	t.Run("uint64_max_is_valid_syntax", func(t *testing.T) {
		called := false
		r := Handler(Business{
			get: func(_ context.Context, id uint64) (*service.KnowledgeDraftDetail, error) {
				called = true
				Equal(t, id, ^uint64(0))
				return nil, service.ErrKnowledgeDraftNotFound
			},
		})
		w := Request(r, "GET", "/versions/18446744073709551615", "", "", false)
		WantError(t, w, 404, "knowledge_draft_not_found")
		Equal(t, called, true)
	})
}

func TestKnowledgeHTTPErrorContract(t *testing.T) {
	cases := []struct {
		err     error
		status  int
		code    string
		message string
	}{
		{service.ErrInvalidArgument, 400, "invalid_argument", "invalid request"},
		{service.ErrRuleCategoryNotFound, 404, "rule_category_not_found", "rule category not found"},
		{service.ErrKnowledgeDraftNotFound, 404, "knowledge_draft_not_found", "knowledge draft not found"},
		{service.ErrKnowledgeDocumentExists, 409, "knowledge_document_exists", "knowledge document already exists"},
		{errors.New("SQL secret: INSERT INTO private_table"), 500, "internal_error", "internal server error"},
		{context.DeadlineExceeded, 504, "request_timeout", "request timed out"},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			wrapped := fmt.Errorf("outer: %w", tc.err)
			r := Handler(Business{
				create: func(context.Context, service.CreateKnowledgeDraftInput) (*model.RuleVersion, error) {
					return nil, wrapped
				},
				get: func(context.Context, uint64) (*service.KnowledgeDraftDetail, error) {
					return nil, wrapped
				},
			})
			for _, method := range []string{"POST", "GET"} {
				path, body := "/versions/1", ""
				if method == "POST" {
					path, body = "/documents", JSON(t, Payload(Input()))
				}
				w := Request(r, method, path, body, "", false)
				WantJSON(t, w, tc.status, map[string]string{
					"code": tc.code, "message": tc.message,
				})
			}
		})
	}
}

func TestKnowledgeHTTPBodyLimit(t *testing.T) {
	valid := JSON(t, Payload(Input()))
	for _, unknown := range []bool{false, true} {
		for _, size := range []int{65536, 65537} {
			t.Run(fmt.Sprintf("bytes_%d_unknown_%v", size, unknown), func(t *testing.T) {
				calls := 0
				r := Handler(Business{
					create: func(context.Context, service.CreateKnowledgeDraftInput) (*model.RuleVersion, error) {
						calls++
						return &model.RuleVersion{
							ID: 22, DocumentID: 11, Version: "test-v1",
							Status: model.RuleVersionStatusDraft,
						}, nil
					},
				})
				body := valid + strings.Repeat(" ", size-len(valid))
				Equal(t, len(body), size)
				w := Request(r, "POST", "/documents", body, "", unknown)
				if size == 65536 {
					WantJSON(t, w, 201, map[string]any{
						"document_id": 11, "version_id": 22,
						"version": "test-v1", "status": "draft",
					})
					Equal(t, calls, 1)
				} else {
					WantError(t, w, 413, "request_too_large")
					Equal(t, calls, 0)
				}
			})
		}
	}
}

func TestKnowledgeHTTPVerificationSummary(t *testing.T) {
	at := time.Date(2026, 10, 27, 2, 0, 0, 0, time.UTC)
	evidence := "https://www.bilibili.com/test-only"

	cases := []struct {
		name                 string
		verificationVersion  uint64
		verificationRevision uint64
		wantMatch            bool
	}{
		{"old_revision", 22, 3, false},
		{"current_revision", 22, 4, true},
		{"different_version", 99, 4, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			r := Handler(Business{
				get: func(
					ctx context.Context,
					id uint64,
				) (*service.KnowledgeDraftDetail, error) {
					calls++
					Equal(t, id, uint64(22))
					Equal(t, ctx.Value(ContextKey{}), "request-marker")

					return &service.KnowledgeDraftDetail{
						Version: &model.RuleVersion{
							ID:              22,
							DocumentID:      11,
							Version:         "test-v1",
							ContentRevision: 4,
							CategoryID:      7,
							Title:           "虚构测试",
							Content:         "虚构全文",
							SourceURL:       "https://www.bilibili.com/test-only",
							Status:          model.RuleVersionStatusDraft,
							Document: model.KnowledgeDocument{
								PublicationRevision: 99,
							},
						},
						Articles: []model.RuleArticle{
							{
								ID:           41,
								VersionID:    22,
								ArticleOrder: 1,
								Content:      "虚构条款",
							},
						},
						Verification: &model.RuleVerification{
							ID:                      31,
							VersionID:               tc.verificationVersion,
							VerifiedContentRevision: tc.verificationRevision,
							VerifiedBy:              9,
							VerifiedAt:              at,
							VerificationNote:        "虚构测试核验依据",
							EvidenceURL:             &evidence,
							SnapshotSchemaVersion:   1,
							ContentSnapshot:         "must-not-leak",
						},
					}, nil
				},
			})

			w := Request(r, "GET", "/versions/22", "", "", false)
			WantJSON(t, w, 200, map[string]any{
				"document_id":         11,
				"version_id":          22,
				"version":             "test-v1",
				"content_revision":    4,
				"category_id":         7,
				"revision_type":       "",
				"revision_note":       "",
				"revision_source_url": nil,
				"title":               "虚构测试",
				"content":             "虚构全文",
				"source_url":          "https://www.bilibili.com/test-only",
				"status":              "draft",
				"effective_at":        nil,
				"expires_at":          nil,
				"articles": []map[string]any{
					{
						"article_id":     41,
						"article_order":  1,
						"section_path":   "",
						"article_number": "",
						"content":        "虚构条款",
					},
				},
				"verification": map[string]any{
					"verification_id":           31,
					"verified_content_revision": tc.verificationRevision,
					"verified_by":               9,
					"verified_at":               at,
					"verification_note":         "虚构测试核验依据",
					"evidence_url":              evidence,
					"matches_current_content":   tc.wantMatch,
				},
			})
			Equal(t, calls, 1)
		})
	}
}
