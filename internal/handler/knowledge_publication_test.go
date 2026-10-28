package handler_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
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

const publicationDocumentPath = "/api/v1/admin/knowledge/documents/11"
const publicationPublishPath = publicationDocumentPath + "/publish"
const publicationCurrentPath = "/api/v1/knowledge/documents/11/current"
const publicationJSON = `{"version_id":21,"expected_publication_revision":0}`

// 复用既有managementBusiness的基础KnowledgeService方法。
// 发布能力由下面三个方法提供；未配置的方法被调用时直接使测试失败。
type publicationBusiness struct {
	managementBusiness

	publish func(
		context.Context, uint64, service.PublishKnowledgeVersionInput,
	) (*service.KnowledgePublicationResult, error)

	current func(
		context.Context, uint64,
	) (*service.KnowledgeCurrentDetail, error)

	state func(
		context.Context, uint64,
	) (*service.KnowledgeDocumentState, error)
}

var _ handler.KnowledgeService = (*publicationBusiness)(nil)
var _ handler.KnowledgePublicationService = (*publicationBusiness)(nil)

func (s *publicationBusiness) PublishVersion(
	ctx context.Context,
	operatorID uint64,
	input service.PublishKnowledgeVersionInput,
) (*service.KnowledgePublicationResult, error) {
	if s.publish == nil {
		panic("request must not reach publication service")
	}
	return s.publish(ctx, operatorID, input)
}

func (s *publicationBusiness) GetCurrentByDocumentID(
	ctx context.Context, id uint64,
) (*service.KnowledgeCurrentDetail, error) {
	if s.current == nil {
		panic("request must not reach current knowledge service")
	}
	return s.current(ctx, id)
}

func (s *publicationBusiness) GetDocumentState(
	ctx context.Context, id uint64,
) (*service.KnowledgeDocumentState, error) {
	if s.state == nil {
		panic("request must not reach document state service")
	}
	return s.state(ctx, id)
}

// 使用真实JWT、角色检查、请求体限制和Handler；业务服务及用户读取使用替身。
// 这不是router.New，实际应用路由的组装会在第三板块单独验证。
func publicationRouter(
	t *testing.T, role string, business handler.KnowledgeService,
) (*gin.Engine, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	tokens, err := auth.NewTokenManager(
		"0123456789abcdef0123456789abcdef",
		"knowledge-publication-http-test",
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

	h := handler.NewKnowledgeHandler(business)
	users := managementUserReader{role: role}

	admin := protected.Group("/admin/knowledge")
	admin.Use(middleware.RequireRoles(users, model.RoleAdmin))
	admin.POST("/documents/:document_id/publish", h.Publish)
	admin.GET("/documents/:document_id", h.GetDocumentState)

	current := protected.Group("/knowledge")
	current.Use(middleware.RequireRoles(
		users, model.RoleUser, model.RoleSupport, model.RoleAdmin,
	))
	current.GET("/documents/:document_id/current", h.GetCurrent)

	return r, token
}

func publicationCheckContext(t *testing.T, ctx context.Context) {
	t.Helper()
	if ctx.Value(managementContextKey{}) != "request-context" {
		t.Fatal("request context was not forwarded")
	}
}

func TestKnowledgePublicationHTTPSuccess(t *testing.T) {
	at := time.Date(2026, 10, 27, 2, 0, 0, 123000000, time.UTC)
	publishCalls, currentCalls, stateCalls := 0, 0, 0

	business := &publicationBusiness{
		publish: func(
			ctx context.Context,
			operatorID uint64,
			input service.PublishKnowledgeVersionInput,
		) (*service.KnowledgePublicationResult, error) {
			publishCalls++
			publicationCheckContext(t, ctx)

			want := service.PublishKnowledgeVersionInput{
				DocumentID:                  11,
				VersionID:                   21,
				ExpectedPublicationRevision: 0,
			}
			if operatorID != 9 || input != want {
				t.Fatalf("operator=%d input=%+v want=%+v",
					operatorID, input, want)
			}
			return &service.KnowledgePublicationResult{
				DocumentID: 11, VersionID: 21,
				PublicationRevision: 1, PublishedAt: at,
			}, nil
		},
		current: func(
			ctx context.Context, id uint64,
		) (*service.KnowledgeCurrentDetail, error) {
			currentCalls++
			publicationCheckContext(t, ctx)
			if id != 11 {
				t.Fatalf("document ID=%d want=11", id)
			}
			return &service.KnowledgeCurrentDetail{
				Version: &model.RuleVersion{
					ID: 21, DocumentID: 11, Version: "test-v1",
					CategoryID: 7, Title: "虚构测试标题",
					Content:     "虚构全文\n保留换行",
					SourceURL:   "https://www.bilibili.com/test-only",
					EffectiveAt: &at,
					ExpiresAt:   nil,

					// 这些内部字段不能被直接序列化到普通响应。
					ContentRevision: 99,
					Status:          model.RuleVersionStatusPublished,
					CreatedAt:       at,
					UpdatedAt:       at,
					PublishedAt:     &at,
				},
				Articles: []model.RuleArticle{
					{
						ID: 102, VersionID: 21, ArticleOrder: 1,
						SectionPath: "第一章", ArticleNumber: "一",
						Content: "虚构第一条", CreatedAt: at,
					},
					{
						ID: 101, VersionID: 21, ArticleOrder: 2,
						SectionPath: "第二章", ArticleNumber: "二",
						Content: "虚构第二条", CreatedAt: at,
					},
				},
			}, nil
		},
		state: func(
			ctx context.Context, id uint64,
		) (*service.KnowledgeDocumentState, error) {
			stateCalls++
			publicationCheckContext(t, ctx)
			if id != 11 {
				t.Fatalf("document ID=%d want=11", id)
			}
			// 无当前指针时仍能取得发布修订，不能依赖当前规则查询。
			return &service.KnowledgeDocumentState{
				DocumentID: 11, CurrentVersionID: nil,
				PublicationRevision: 8,
			}, nil
		},
	}
	r, token := publicationRouter(t, model.RoleAdmin, business)

	// 查询参数和伪造身份请求头不能覆盖JWT中的管理员9。
	req := httptest.NewRequest(
		http.MethodPost,
		publicationPublishPath+"?operator_id=999&role=admin",
		strings.NewReader(publicationJSON),
	)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-ID", "999")
	req = req.WithContext(context.WithValue(
		req.Context(), managementContextKey{}, "request-context",
	))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	managementWantJSON(t, w, 200, map[string]any{
		"document_id": 11, "version_id": 21,
		"publication_revision": 1, "published_at": at,
	})

	w = managementRequest(
		r, "GET", publicationCurrentPath, "", token, false,
	)
	// 整份JSON精确比较，同时拒绝多余内部字段。
	// ID刻意与条款顺序相反，确保Handler保留服务返回的顺序。
	managementWantJSON(t, w, 200, map[string]any{
		"document_id": 11, "version_id": 21, "version": "test-v1",
		"category_id": 7, "title": "虚构测试标题",
		"content":      "虚构全文\n保留换行",
		"source_url":   "https://www.bilibili.com/test-only",
		"effective_at": at, "expires_at": nil,
		"articles": []map[string]any{
			{
				"article_id": 102, "article_order": 1,
				"section_path": "第一章", "article_number": "一",
				"content": "虚构第一条",
			},
			{
				"article_id": 101, "article_order": 2,
				"section_path": "第二章", "article_number": "二",
				"content": "虚构第二条",
			},
		},
	})

	w = managementRequest(
		r, "GET", publicationDocumentPath, "", token, false,
	)
	managementWantJSON(t, w, 200, map[string]any{
		"document_id": 11, "current_version_id": nil,
		"publication_revision": 8,
	})

	if publishCalls != 1 || currentCalls != 1 || stateCalls != 1 {
		t.Fatalf("calls: publish=%d current=%d state=%d; want 1 each",
			publishCalls, currentCalls, stateCalls)
	}
}

func TestKnowledgePublicationHTTPRejectsMalformedRequests(t *testing.T) {
	// 所有业务回调都未配置：非法输入若进入服务，测试立即失败。
	r, token := publicationRouter(
		t, model.RoleAdmin, &publicationBusiness{},
	)

	bodies := []string{
		"", "null", "{", "[]", "{}",
		publicationJSON + "{}",
		`{"version_id":21}`,
		`{"version_id":21,"expected_publication_revision":null}`,
		`{"expected_publication_revision":0}`,
	}
	for _, value := range []string{
		"0", "-1", "null", `"21"`, "1.5", "18446744073709551616",
	} {
		bodies = append(bodies, fmt.Sprintf(
			`{"version_id":%s,"expected_publication_revision":0}`, value,
		))
	}
	for _, value := range []string{
		"-1", `"0"`, "1.5", "18446744073709551616",
	} {
		bodies = append(bodies, fmt.Sprintf(
			`{"version_id":21,"expected_publication_revision":%s}`, value,
		))
	}
	for _, field := range []string{
		`"operator_id":999`,
		`"document_id":999`,
		`"status":"published"`,
		`"published_at":"2026-10-27T00:00:00Z"`,
		`"verified_by":999`,
		`"publication_revision":1`,
	} {
		bodies = append(bodies,
			strings.Replace(publicationJSON, "{", "{"+field+",", 1))
	}

	for i, body := range bodies {
		t.Run(fmt.Sprintf("body_%02d", i), func(t *testing.T) {
			w := managementRequest(
				r, "POST", publicationPublishPath, body, token, false,
			)
			managementWantError(t, w, 400, "invalid_argument")
		})
	}

	for _, id := range []string{
		"abc", "0", "-1", "18446744073709551616",
	} {
		t.Run("path_"+id, func(t *testing.T) {
			for _, endpoint := range []struct {
				method string
				path   string
				body   string
			}{
				{"POST", publicationPublishPath, publicationJSON},
				{"GET", publicationDocumentPath, ""},
				{"GET", publicationCurrentPath, ""},
			} {
				path := strings.Replace(endpoint.path, "/11", "/"+id, 1)
				w := managementRequest(
					r, endpoint.method, path, endpoint.body, token, false,
				)
				managementWantError(t, w, 400, "invalid_argument")
			}
		})
	}
}

func TestKnowledgePublicationHTTPErrorMapping(t *testing.T) {
	cases := []struct {
		err     error
		status  int
		code    string
		message string
	}{
		{service.ErrInvalidArgument, 400,
			"invalid_argument", "invalid request"},
		{service.ErrKnowledgeDocumentNotFound, 404,
			"knowledge_document_not_found", "knowledge document not found"},
		{service.ErrKnowledgeVersionNotFound, 404,
			"knowledge_version_not_found", "knowledge version not found"},
		{service.ErrKnowledgePublicationConflict, 409,
			"knowledge_publication_conflict", "knowledge publication revision conflict"},
		{service.ErrKnowledgeCurrentVersionExists, 409,
			"knowledge_current_version_exists", "knowledge document already has a current version"},
		{service.ErrKnowledgeVersionNotPublishable, 409,
			"knowledge_version_not_publishable", "knowledge version is not publishable"},
		{service.ErrKnowledgeVerificationRequired, 409,
			"knowledge_verification_required", "current knowledge content requires verification"},
		{service.ErrKnowledgeEffectiveTimeRequired, 409,
			"knowledge_effective_time_required", "knowledge effective time is required"},
		{service.ErrKnowledgeNotYetEffective, 409,
			"knowledge_not_yet_effective", "knowledge version is not yet effective"},
		{service.ErrKnowledgeExpired, 409,
			"knowledge_expired", "knowledge version has expired"},
		{errors.New("SQL secret password=private"), 500,
			"internal_error", "internal server error"},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			calls := 0
			business := &publicationBusiness{
				publish: func(
					context.Context, uint64, service.PublishKnowledgeVersionInput,
				) (*service.KnowledgePublicationResult, error) {
					calls++
					return nil, fmt.Errorf("repository detail: %w", tc.err)
				},
			}
			r, token := publicationRouter(t, model.RoleAdmin, business)
			w := managementRequest(
				r, "POST", publicationPublishPath, publicationJSON, token, false,
			)
			managementWantJSON(t, w, tc.status, map[string]string{
				"code": tc.code, "message": tc.message,
			})
			if calls != 1 {
				t.Fatalf("publication calls=%d want=1", calls)
			}
		})
	}
}

func TestKnowledgePublicationHTTPReadErrors(t *testing.T) {
	cases := []struct {
		name   string
		path   string
		err    error
		status int
		code   string
	}{
		{"current_document_missing", publicationCurrentPath,
			service.ErrKnowledgeDocumentNotFound, 404, "knowledge_document_not_found"},
		{"current_version_missing", publicationCurrentPath,
			service.ErrKnowledgeCurrentVersionNotFound, 404, "knowledge_current_version_not_found"},
		{"state_document_missing", publicationDocumentPath,
			service.ErrKnowledgeDocumentNotFound, 404, "knowledge_document_not_found"},
		{"current_internal", publicationCurrentPath,
			errors.New("SQL secret"), 500, "internal_error"},
		{"state_internal", publicationDocumentPath,
			errors.New("SQL secret"), 500, "internal_error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			wrapped := fmt.Errorf("outer: %w", tc.err)
			business := &publicationBusiness{
				current: func(context.Context, uint64) (
					*service.KnowledgeCurrentDetail, error,
				) {
					calls++
					return nil, wrapped
				},
				state: func(context.Context, uint64) (
					*service.KnowledgeDocumentState, error,
				) {
					calls++
					return nil, wrapped
				},
			}
			r, token := publicationRouter(t, model.RoleAdmin, business)
			w := managementRequest(r, "GET", tc.path, "", token, false)
			managementWantError(t, w, tc.status, tc.code)
			if tc.status == 500 {
				managementWantJSON(t, w, 500, map[string]string{
					"code": "internal_error", "message": "internal server error",
				})
			}
			if calls != 1 {
				t.Fatalf("read calls=%d want=1", calls)
			}
		})
	}
}

func TestKnowledgePublicationHTTPRejectsEmptyResults(t *testing.T) {
	// nil,nil不能被误认为业务成功；缺少发布能力也应返回固定500。
	for _, missingCapability := range []bool{false, true} {
		t.Run(fmt.Sprintf("missing_capability_%v", missingCapability), func(t *testing.T) {
			var business handler.KnowledgeService = &publicationBusiness{
				publish: func(context.Context, uint64, service.PublishKnowledgeVersionInput) (
					*service.KnowledgePublicationResult, error,
				) {
					return nil, nil
				},
				current: func(context.Context, uint64) (
					*service.KnowledgeCurrentDetail, error,
				) {
					return nil, nil
				},
				state: func(context.Context, uint64) (
					*service.KnowledgeDocumentState, error,
				) {
					return nil, nil
				},
			}
			if missingCapability {
				business = &managementBusiness{}
			}
			r, token := publicationRouter(t, model.RoleAdmin, business)
			for _, endpoint := range []struct {
				method, path, body string
			}{
				{"POST", publicationPublishPath, publicationJSON},
				{"GET", publicationCurrentPath, ""},
				{"GET", publicationDocumentPath, ""},
			} {
				w := managementRequest(
					r, endpoint.method, endpoint.path, endpoint.body, token, false,
				)
				managementWantJSON(t, w, 500, map[string]string{
					"code": "internal_error", "message": "internal server error",
				})
			}
		})
	}

	t.Run("detail_without_version", func(t *testing.T) {
		business := &publicationBusiness{
			current: func(context.Context, uint64) (
				*service.KnowledgeCurrentDetail, error,
			) {
				return &service.KnowledgeCurrentDetail{}, nil
			},
		}
		r, token := publicationRouter(t, model.RoleAdmin, business)
		w := managementRequest(r, "GET", publicationCurrentPath, "", token, false)
		managementWantJSON(t, w, 500, map[string]string{
			"code": "internal_error", "message": "internal server error",
		})
	})
}

func TestKnowledgePublicationHTTPPermissions(t *testing.T) {
	for _, role := range []string{
		model.RoleAdmin, model.RoleUser, model.RoleSupport,
	} {
		t.Run(role, func(t *testing.T) {
			// 普通用户和客服：管理入口应在调用业务前拒绝。
			// 管理员的成功管理路径已由HTTPSuccess覆盖。
			if role != model.RoleAdmin {
				r, token := publicationRouter(t, role, &publicationBusiness{})
				for _, endpoint := range []struct {
					method, path, body string
				}{
					{"POST", publicationPublishPath, publicationJSON},
					{"GET", publicationDocumentPath, ""},
				} {
					w := managementRequest(
						r, endpoint.method, endpoint.path, endpoint.body, token, false,
					)
					managementWantError(t, w, 403, "forbidden")
				}
			}

			// 三种登录角色都可以进入当前查询业务。
			calls := 0
			business := &publicationBusiness{
				current: func(context.Context, uint64) (
					*service.KnowledgeCurrentDetail, error,
				) {
					calls++
					return &service.KnowledgeCurrentDetail{
						Version: &model.RuleVersion{ID: 21, DocumentID: 11},
					}, nil
				},
			}
			r, token := publicationRouter(t, role, business)
			w := managementRequest(r, "GET", publicationCurrentPath, "", token, false)
			if w.Code != 200 || calls != 1 {
				t.Fatalf("status=%d calls=%d body=%s", w.Code, calls, w.Body.String())
			}
		})
	}

	for _, token := range []string{"", "invalid-token"} {
		t.Run("unauthorized_"+token, func(t *testing.T) {
			r, _ := publicationRouter(t, model.RoleAdmin, &publicationBusiness{})
			for _, endpoint := range []struct {
				method, path, body string
			}{
				{"POST", publicationPublishPath, publicationJSON},
				{"GET", publicationDocumentPath, ""},
				{"GET", publicationCurrentPath, ""},
			} {
				w := managementRequest(
					r, endpoint.method, endpoint.path, endpoint.body, token, false,
				)
				managementWantError(t, w, 401, "unauthorized")
				if w.Header().Get("WWW-Authenticate") != "Bearer" {
					t.Fatal("missing Bearer challenge")
				}
			}
		})
	}
}

func TestKnowledgePublicationHTTPBodyLimit(t *testing.T) {
	for _, unknownLength := range []bool{false, true} {
		for _, size := range []int{65536, 65537} {
			t.Run(fmt.Sprintf("unknown_%v_size_%d", unknownLength, size), func(t *testing.T) {
				calls := 0
				business := &publicationBusiness{
					publish: func(
						context.Context, uint64, service.PublishKnowledgeVersionInput,
					) (*service.KnowledgePublicationResult, error) {
						calls++
						return &service.KnowledgePublicationResult{
							DocumentID: 11, VersionID: 21, PublicationRevision: 1,
						}, nil
					},
				}
				r, token := publicationRouter(t, model.RoleAdmin, business)

				// JSON后补空白：语义合法，大小精确可控。
				// 第二次Decode必须继续读取到EOF，超限不能漏检。
				body := publicationJSON + strings.Repeat(" ", size-len(publicationJSON))
				w := managementRequest(
					r, "POST", publicationPublishPath, body, token, unknownLength,
				)
				if size == 65536 {
					if w.Code != 200 || calls != 1 {
						t.Fatalf("boundary status=%d calls=%d body=%s",
							w.Code, calls, w.Body.String())
					}
				} else {
					managementWantError(t, w, 413, "request_too_large")
					if calls != 0 {
						t.Fatal("oversized body reached business service")
					}
				}
			})
		}
	}
}
