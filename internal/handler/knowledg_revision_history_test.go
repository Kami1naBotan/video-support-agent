package handler_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"video-support-agent/internal/auth"
	"video-support-agent/internal/handler"
	"video-support-agent/internal/middleware"
	"video-support-agent/internal/model"
	"video-support-agent/internal/service"

	"github.com/gin-gonic/gin"
)

// 全部内容均为内存中的虚构测试数据，不连接数据库。
const revisionHTTPBody = `{
	"version":"v2",
	"revision_type":"editorial",
	"revision_note":"虚构录入纠错依据",
	"revision_source_url":null,
	"category_id":7,
	"title":"虚构测试标题",
	"content":"虚构全文\n保留换行",
	"source_url":"https://www.bilibili.com/test-only",
	"effective_at":null,
	"expires_at":null,
	"articles":[
		{"section_path":"测试章节","article_number":"第一条","content":"虚构条款"}
	]
}`

const revisionHTTPPath = "/api/v1/admin/knowledge/documents/11/versions"
const historyHTTPPath = "/api/v1/knowledge/versions/21"
const historyArticleHTTPPath = "/api/v1/knowledge/versions/21/articles/91"

type revisionHistoryBusiness struct {
	managementBusiness

	createRevision func(context.Context, service.CreateKnowledgeRevisionInput) (
		*model.RuleVersion, error)
	history func(context.Context, uint64) (
		*service.KnowledgeHistoryDetail, error)
	article func(context.Context, uint64, uint64) (
		*service.KnowledgeHistoryArticleDetail, error)
	detail func(context.Context, uint64) (
		*service.KnowledgeDraftDetail, error)
}

func (s *revisionHistoryBusiness) CreateRevision(
	ctx context.Context, input service.CreateKnowledgeRevisionInput,
) (*model.RuleVersion, error) {
	if s.createRevision == nil {
		panic("request must not reach revision service")
	}
	return s.createRevision(ctx, input)
}

func (s *revisionHistoryBusiness) GetHistoryByVersionID(
	ctx context.Context, id uint64,
) (*service.KnowledgeHistoryDetail, error) {
	if s.history == nil {
		panic("request must not reach history service")
	}
	return s.history(ctx, id)
}

func (s *revisionHistoryBusiness) GetHistoryArticle(
	ctx context.Context, versionID, articleID uint64,
) (*service.KnowledgeHistoryArticleDetail, error) {
	if s.article == nil {
		panic("request must not reach history article service")
	}
	return s.article(ctx, versionID, articleID)
}

func (s *revisionHistoryBusiness) GetDraftDetailByID(
	ctx context.Context, id uint64,
) (*service.KnowledgeDraftDetail, error) {
	if s.detail == nil {
		panic("request must not reach admin detail service")
	}
	return s.detail(ctx, id)
}

// 这是Handler测试路由；实际router.New的组装在板块4验证。
func revisionHistoryRouter(
	t *testing.T, role string, business *revisionHistoryBusiness,
) (*gin.Engine, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	tokens, err := auth.NewTokenManager(
		"0123456789abcdef0123456789abcdef",
		"knowledge-revision-history-test",
		time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	token, err := tokens.Issue(9)
	if err != nil {
		t.Fatal(err)
	}

	r := gin.New()
	r.Use(middleware.BodyLimit(65536))

	protected := r.Group("/api/v1")
	protected.Use(middleware.JWTAuth(tokens))
	users := managementUserReader{role: role}
	h := handler.NewKnowledgeHandler(business)

	admin := protected.Group("/admin/knowledge")
	admin.Use(middleware.RequireRoles(users, model.RoleAdmin))
	admin.POST("/documents/:document_id/versions", h.CreateRevision)
	admin.GET("/versions/:version_id", h.GetByID)

	public := protected.Group("/knowledge")
	public.Use(middleware.RequireRoles(
		users, model.RoleUser, model.RoleSupport, model.RoleAdmin,
	))
	public.GET("/versions/:version_id", h.GetHistory)
	public.GET("/versions/:version_id/articles/:article_id", h.GetHistoryArticle)

	return r, token
}

func revisionHistoryCheckContext(t *testing.T, ctx context.Context) {
	t.Helper()
	if ctx.Value(managementContextKey{}) != "request-context" {
		t.Fatal("request context was not propagated")
	}
}

func revisionCreatedVersion() *model.RuleVersion {
	return &model.RuleVersion{
		ID: 21, DocumentID: 11, Version: "v2",
		Status:          model.RuleVersionStatusDraft,
		ContentRevision: 1,
		Content:         "must not leak in creation response",
	}
}

func revisionCreatedJSON() map[string]any {
	return map[string]any{
		"document_id": 11, "version_id": 21,
		"version": "v2", "status": "draft",
	}
}

func TestKnowledgeRevisionHTTPMapping(t *testing.T) {
	for _, withDates := range []bool{false, true} {
		t.Run(fmt.Sprintf("dates_%v", withDates), func(t *testing.T) {
			body := revisionHTTPBody
			want := service.CreateKnowledgeRevisionInput{
				DocumentID:   11,
				Version:      "v2",
				RevisionType: model.RuleRevisionTypeEditorial,
				RevisionNote: "虚构录入纠错依据",
				CategoryID:   7,
				Title:        "虚构测试标题",
				Content:      "虚构全文\n保留换行",
				SourceURL:    "https://www.bilibili.com/test-only",
				Articles: []service.CreateRuleArticleInput{{
					SectionPath:   "测试章节",
					ArticleNumber: "第一条",
					Content:       "虚构条款",
				}},
			}
			if withDates {
				start := time.Date(2026, 10, 27, 2, 0, 0, 0, time.UTC)
				end := start.Add(24 * time.Hour)
				evidence := "https://www.bilibili.com/test-evidence"
				want.EffectiveAt = &start
				want.ExpiresAt = &end
				want.RevisionSourceURL = &evidence
				body = strings.Replace(body, `"effective_at":null`,
					`"effective_at":"2026-10-27T02:00:00Z"`, 1)
				body = strings.Replace(body, `"expires_at":null`,
					`"expires_at":"2026-10-28T02:00:00Z"`, 1)
				body = strings.Replace(body, `"revision_source_url":null`,
					`"revision_source_url":"https://www.bilibili.com/test-evidence"`, 1)
			}

			calls := 0
			b := &revisionHistoryBusiness{
				createRevision: func(
					ctx context.Context, got service.CreateKnowledgeRevisionInput,
				) (*model.RuleVersion, error) {
					calls++
					revisionHistoryCheckContext(t, ctx)
					if !reflect.DeepEqual(got, want) {
						t.Fatalf("input=%+v want=%+v", got, want)
					}
					return revisionCreatedVersion(), nil
				},
			}
			r, token := revisionHistoryRouter(t, model.RoleAdmin, b)
			w := managementRequest(r, "POST", revisionHTTPPath, body, token, false)
			managementWantJSON(t, w, 201, revisionCreatedJSON())
			if calls != 1 {
				t.Fatalf("calls=%d want=1", calls)
			}
		})
	}
}

func TestKnowledgeRevisionHTTPRejectsMalformedBody(t *testing.T) {
	bodies := map[string]string{
		"empty":           "",
		"null":            "null",
		"broken_json":     "{",
		"array":           "[]",
		"multiple_json":   revisionHTTPBody + "{}",
		"invalid_time":    `{"effective_at":"tomorrow"}`,
		"wrong_category":  `{"category_id":"7"}`,
		"negative_id":     `{"category_id":-1}`,
		"overflow_id":     `{"category_id":18446744073709551616}`,
		"unknown_article": `{"articles":[{"article_id":91}]}`,
	}
	for _, field := range []string{
		"document_id", "version_id", "status", "content_revision",
		"publication_revision", "verified_by", "operator_id",
		"verification", "published_at",
	} {
		bodies[field] = strings.Replace(
			revisionHTTPBody, "{", fmt.Sprintf(`{%q:1,`, field), 1,
		)
	}

	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			r, token := revisionHistoryRouter(
				t, model.RoleAdmin, &revisionHistoryBusiness{},
			)
			w := managementRequest(r, "POST", revisionHTTPPath, body, token, false)
			managementWantError(t, w, 400, "invalid_argument")
		})
	}
}

func TestKnowledgeRevisionHTTPBodyLimit(t *testing.T) {
	for _, unknownLength := range []bool{false, true} {
		for _, size := range []int{65536, 65537} {
			t.Run(fmt.Sprintf("%d_unknown_%v", size, unknownLength), func(t *testing.T) {
				calls := 0
				b := &revisionHistoryBusiness{
					createRevision: func(
						context.Context, service.CreateKnowledgeRevisionInput,
					) (*model.RuleVersion, error) {
						calls++
						return revisionCreatedVersion(), nil
					},
				}
				r, token := revisionHistoryRouter(t, model.RoleAdmin, b)
				body := revisionHTTPBody +
					strings.Repeat(" ", size-len(revisionHTTPBody))
				w := managementRequest(
					r, "POST", revisionHTTPPath, body, token, unknownLength,
				)
				wantCalls := 0
				if size == 65536 {
					wantCalls = 1
					managementWantJSON(t, w, 201, revisionCreatedJSON())
				} else {
					managementWantError(t, w, 413, "request_too_large")
				}
				if calls != wantCalls {
					t.Fatalf("calls=%d want=%d", calls, wantCalls)
				}
			})
		}
	}
}

func TestKnowledgeRevisionHistoryHTTPRejectsInvalidIDs(t *testing.T) {
	for _, id := range []string{"0", "-1", "abc", "18446744073709551616"} {
		cases := []struct {
			method string
			path   string
		}{
			{"POST", "/api/v1/admin/knowledge/documents/" + id + "/versions"},
			{"GET", "/api/v1/knowledge/versions/" + id},
			{"GET", "/api/v1/knowledge/versions/" + id + "/articles/91"},
			{"GET", historyHTTPPath + "/articles/" + id},
		}
		for i, tc := range cases {
			t.Run(fmt.Sprintf("%s_route_%d", id, i), func(t *testing.T) {
				r, token := revisionHistoryRouter(
					t, model.RoleAdmin, &revisionHistoryBusiness{},
				)
				w := managementRequest(
					r, tc.method, tc.path, revisionHTTPBody, token, false,
				)
				managementWantError(t, w, 400, "invalid_argument")
			})
		}
	}
}

func TestKnowledgeRevisionHistoryHTTPErrorMapping(t *testing.T) {
	cases := []struct {
		name   string
		target string
		err    error
		status int
		code   string
	}{
		{"invalid", "create", service.ErrInvalidArgument, 400, "invalid_argument"},
		{"document", "create", service.ErrKnowledgeDocumentNotFound, 404, "knowledge_document_not_found"},
		{"category", "create", service.ErrRuleCategoryNotFound, 404, "rule_category_not_found"},
		{"duplicate", "create", service.ErrKnowledgeVersionExists, 409, "knowledge_version_exists"},
		{"version", "history", service.ErrKnowledgeVersionNotFound, 404, "knowledge_version_not_found"},
		{"article_version", "article", service.ErrKnowledgeVersionNotFound, 404, "knowledge_version_not_found"},
		{"article", "article", service.ErrKnowledgeArticleNotFound, 404, "knowledge_article_not_found"},
	}

	// 三个入口都检查内部错误脱敏，以及nil结果保护。
	for _, target := range []string{"create", "history", "article"} {
		cases = append(cases, struct {
			name   string
			target string
			err    error
			status int
			code   string
		}{target + "_internal", target, errors.New("secret SQL password"), 500, "internal_error"})
		cases = append(cases, struct {
			name   string
			target string
			err    error
			status int
			code   string
		}{target + "_nil", target, nil, 500, "internal_error"})
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.err
			if err != nil {
				err = fmt.Errorf("private database detail: %w", err)
			}
			b := &revisionHistoryBusiness{
				createRevision: func(
					context.Context, service.CreateKnowledgeRevisionInput,
				) (*model.RuleVersion, error) {
					return nil, err
				},
				history: func(context.Context, uint64) (*service.KnowledgeHistoryDetail, error) {
					return nil, err
				},
				article: func(context.Context, uint64, uint64) (*service.KnowledgeHistoryArticleDetail, error) {
					return nil, err
				},
			}
			r, token := revisionHistoryRouter(t, model.RoleAdmin, b)
			method, path, body := "GET", historyHTTPPath, ""
			switch tc.target {
			case "create":
				method, path, body = "POST", revisionHTTPPath, revisionHTTPBody
			case "article":
				path = historyArticleHTTPPath
			}
			w := managementRequest(r, method, path, body, token, false)
			if tc.status == 500 {
				managementWantJSON(t, w, 500, map[string]string{
					"code": "internal_error", "message": "internal server error",
				})
			} else {
				managementWantError(t, w, tc.status, tc.code)
				if strings.Contains(w.Body.String(), "private database detail") {
					t.Fatal("wrapped internal detail leaked")
				}
			}
		})
	}
}

func TestKnowledgeHistoryHTTPPublicResponse(t *testing.T) {
	at := time.Date(2026, 10, 27, 2, 0, 0, 0, time.UTC)
	privateURL := "https://www.bilibili.com/internal-evidence"
	version := &model.RuleVersion{
		ID: 21, DocumentID: 11, Version: "v1", CategoryID: 7,
		RevisionType:      model.RuleRevisionTypeEditorial,
		RevisionNote:      "内部修订说明不能公开",
		RevisionSourceURL: &privateURL,
		ContentRevision:   99,
		Title:             "虚构历史标题",
		Content:           "虚构历史全文",
		SourceURL:         "https://www.bilibili.com/test-only",
		Status:            model.RuleVersionStatusPublished,
		EffectiveAt:       &at,
		PublishedAt:       &at,
		CreatedAt:         at,
		UpdatedAt:         at,
		Document:          model.KnowledgeDocument{PublicationRevision: 88},
	}
	// ID故意不递增，避免把条款顺序错误地理解为ID顺序。
	articles := []model.RuleArticle{
		{ID: 91, VersionID: 21, ArticleOrder: 1, SectionPath: "第一章", ArticleNumber: "一", Content: "虚构甲"},
		{ID: 90, VersionID: 21, ArticleOrder: 2, SectionPath: "第二章", ArticleNumber: "", Content: "虚构乙"},
	}
	wantVersion := map[string]any{
		"document_id": 11, "version_id": 21, "version": "v1",
		"category_id": 7, "revision_type": "editorial",
		"title": "虚构历史标题", "source_url": version.SourceURL,
		"effective_at": at, "expires_at": nil, "published_at": at,
	}
	wantArticles := []map[string]any{
		{"article_id": 91, "article_order": 1, "section_path": "第一章", "article_number": "一", "content": "虚构甲"},
		{"article_id": 90, "article_order": 2, "section_path": "第二章", "article_number": "", "content": "虚构乙"},
	}
	states := []service.KnowledgeHistoryState{
		{IsCurrent: true, IsCurrentEffective: true},
		{IsSuperseded: true, IsExpired: true},
		{IsSuperseded: true, IsExpired: true, IsWithdrawn: true},
	}

	for _, role := range []string{model.RoleUser, model.RoleSupport, model.RoleAdmin} {
		for i, state := range states {
			t.Run(fmt.Sprintf("%s_state_%d", role, i), func(t *testing.T) {
				historyCalls, articleCalls := 0, 0
				b := &revisionHistoryBusiness{
					history: func(ctx context.Context, id uint64) (*service.KnowledgeHistoryDetail, error) {
						historyCalls++
						revisionHistoryCheckContext(t, ctx)
						if id != 21 {
							t.Fatalf("versionID=%d", id)
						}
						return &service.KnowledgeHistoryDetail{
							Version: version, Articles: articles, State: state,
						}, nil
					},
					article: func(ctx context.Context, versionID, articleID uint64) (*service.KnowledgeHistoryArticleDetail, error) {
						articleCalls++
						revisionHistoryCheckContext(t, ctx)
						if versionID != 21 || articleID != 91 {
							t.Fatalf("IDs=%d/%d", versionID, articleID)
						}
						return &service.KnowledgeHistoryArticleDetail{
							Version: version, Article: &articles[0], State: state,
						}, nil
					},
				}
				r, token := revisionHistoryRouter(t, role, b)
				wantState := map[string]any{
					"is_current":           state.IsCurrent,
					"is_superseded":        state.IsSuperseded,
					"is_expired":           state.IsExpired,
					"is_withdrawn":         state.IsWithdrawn,
					"is_current_effective": state.IsCurrentEffective,
				}

				w := managementRequest(r, "GET", historyHTTPPath, "", token, false)
				managementWantJSON(t, w, 200, map[string]any{
					"version": wantVersion, "content": "虚构历史全文",
					"articles": wantArticles, "state": wantState,
				})
				w = managementRequest(r, "GET", historyArticleHTTPPath, "", token, false)
				managementWantJSON(t, w, 200, map[string]any{
					"version": wantVersion, "article": wantArticles[0], "state": wantState,
				})
				if historyCalls != 1 || articleCalls != 1 {
					t.Fatalf("calls: history=%d article=%d", historyCalls, articleCalls)
				}
			})
		}
	}
}

func TestKnowledgeRevisionHTTPAdminDetail(t *testing.T) {
	evidence := "https://www.bilibili.com/test-evidence"
	calls := 0
	b := &revisionHistoryBusiness{
		detail: func(ctx context.Context, id uint64) (*service.KnowledgeDraftDetail, error) {
			calls++
			revisionHistoryCheckContext(t, ctx)
			if id != 21 {
				t.Fatalf("versionID=%d", id)
			}
			return &service.KnowledgeDraftDetail{
				Version: &model.RuleVersion{
					ID: 21, DocumentID: 11, Version: "v2",
					CategoryID: 7, ContentRevision: 3,
					RevisionType:      model.RuleRevisionTypeOfficial,
					RevisionNote:      "虚构核验依据",
					RevisionSourceURL: &evidence,
					Title:             "虚构草稿",
					Content:           "虚构全文",
					SourceURL:         "https://www.bilibili.com/test-only",
					Status:            model.RuleVersionStatusDraft,
				},
				Articles: []model.RuleArticle{{
					ID: 91, VersionID: 21, ArticleOrder: 1, Content: "虚构条款",
				}},
			}, nil
		},
	}
	r, token := revisionHistoryRouter(t, model.RoleAdmin, b)
	w := managementRequest(
		r, "GET", "/api/v1/admin/knowledge/versions/21", "", token, false,
	)
	managementWantJSON(t, w, 200, map[string]any{
		"document_id": 11, "version_id": 21, "version": "v2",
		"category_id": 7, "content_revision": 3,
		"revision_type": "official", "revision_note": "虚构核验依据",
		"revision_source_url": evidence,
		"title":               "虚构草稿", "content": "虚构全文",
		"source_url": "https://www.bilibili.com/test-only",
		"status":     "draft", "effective_at": nil, "expires_at": nil,
		"articles": []map[string]any{{
			"article_id": 91, "article_order": 1,
			"section_path": "", "article_number": "", "content": "虚构条款",
		}},
		"verification": nil,
	})
	if calls != 1 {
		t.Fatalf("calls=%d want=1", calls)
	}
}

func TestKnowledgeRevisionHistoryHTTPAuthorization(t *testing.T) {
	paths := []struct {
		method string
		path   string
	}{
		{"POST", revisionHTTPPath},
		{"GET", "/api/v1/admin/knowledge/versions/21"},
		{"GET", historyHTTPPath},
		{"GET", historyArticleHTTPPath},
	}
	for _, tc := range paths {
		t.Run("anonymous_"+tc.path, func(t *testing.T) {
			r, _ := revisionHistoryRouter(
				t, model.RoleAdmin, &revisionHistoryBusiness{},
			)
			w := managementRequest(r, tc.method, tc.path, revisionHTTPBody, "", false)
			managementWantError(t, w, 401, "unauthorized")
		})
	}
	for _, role := range []string{model.RoleUser, model.RoleSupport} {
		for _, tc := range paths[:2] {
			t.Run(role+"_"+tc.path, func(t *testing.T) {
				r, token := revisionHistoryRouter(t, role, &revisionHistoryBusiness{})
				w := managementRequest(
					r, tc.method, tc.path, revisionHTTPBody, token, false,
				)
				managementWantError(t, w, 403, "forbidden")
			})
		}
	}
}
