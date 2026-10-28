package handler_test

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

	"video-support-agent/internal/auth"
	"video-support-agent/internal/handler"
	"video-support-agent/internal/middleware"
	"video-support-agent/internal/model"
	"video-support-agent/internal/service"

	"github.com/gin-gonic/gin"
)

const managementEditJSON = `{
	"expected_content_revision":3,
	"category_id":7,
	"title":"虚构测试标题",
	"content":"虚构全文\n保留换行",
	"source_url":"https://www.bilibili.com/test-only",
	"effective_at":null,
	"expires_at":null,
	"articles":[{
		"section_path":"测试章节",
		"article_number":"第一条",
		"content":"虚构条款"
	}]
}`

const managementVerifyJSON = `{
	"expected_content_revision":3,
	"verification_note":"虚构测试核验",
	"evidence_url":null
}`

const managementPath = "/api/v1/admin/knowledge/versions/21"

type managementContextKey struct{}

type managementBusiness struct {
	edit func(context.Context, service.EditKnowledgeDraftInput) (
		*service.KnowledgeDraftEditResult, error)
	verify func(context.Context, uint64, service.VerifyKnowledgeDraftInput) (
		*model.RuleVerification, error)
}

func (*managementBusiness) CreateDraft(
	context.Context, service.CreateKnowledgeDraftInput,
) (*model.RuleVersion, error) {
	panic("unexpected create")
}

func (*managementBusiness) GetDraftByID(
	context.Context, uint64,
) (*service.KnowledgeDraftDetail, error) {
	panic("unexpected legacy read")
}

func (*managementBusiness) GetDraftDetailByID(
	context.Context, uint64,
) (*service.KnowledgeDraftDetail, error) {
	panic("unexpected detail read")
}

func (s *managementBusiness) EditDraftWithVerification(
	ctx context.Context, input service.EditKnowledgeDraftInput,
) (*service.KnowledgeDraftEditResult, error) {
	if s.edit == nil {
		panic("request must not reach edit service")
	}
	return s.edit(ctx, input)
}

func (s *managementBusiness) VerifyDraft(
	ctx context.Context, id uint64, input service.VerifyKnowledgeDraftInput,
) (*model.RuleVerification, error) {
	if s.verify == nil {
		panic("request must not reach verification service")
	}
	return s.verify(ctx, id, input)
}

type managementUserReader struct {
	role string
}

func (s managementUserReader) Get(
	_ context.Context, id uint64,
) (*model.User, error) {
	return &model.User{ID: id, Role: s.role}, nil
}

func managementRouter(
	t *testing.T, role string, business *managementBusiness,
) (*gin.Engine, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	tokens, err := auth.NewTokenManager(
		"0123456789abcdef0123456789abcdef",
		"knowledge-http-test",
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
	group := r.Group("/api/v1/admin/knowledge")
	group.Use(
		middleware.JWTAuth(tokens),
		middleware.RequireRoles(managementUserReader{role: role}, model.RoleAdmin),
	)
	h := handler.NewKnowledgeHandler(business)
	group.PUT("/versions/:version_id", h.Edit)
	group.POST("/versions/:version_id/verifications", h.Verify)
	return r, token
}

func managementRequest(
	r http.Handler, method, path, body, token string, unknownLength bool,
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
		req.Context(), managementContextKey{}, "request-context",
	))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func managementWantJSON(
	t *testing.T, w *httptest.ResponseRecorder, status int, want any,
) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status=%d want=%d body=%s", w.Code, status, w.Body.String())
	}
	if !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("unexpected content type: %s", w.Header().Get("Content-Type"))
	}
	var gotJSON, wantJSON any
	if err := json.Unmarshal(w.Body.Bytes(), &gotJSON); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &wantJSON); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotJSON, wantJSON) {
		t.Fatalf("got=%s\nwant=%s", w.Body.String(), data)
	}
}

func managementWantError(
	t *testing.T, w *httptest.ResponseRecorder, status int, code string,
) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status=%d want=%d body=%s", w.Code, status, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	message, ok := body["message"].(string)
	if len(body) != 2 || body["code"] != code || !ok || message == "" {
		t.Fatalf("unexpected error body: %s", w.Body.String())
	}
}

func TestKnowledgeManagementHTTPSuccess(t *testing.T) {
	at := time.Date(2026, 10, 27, 2, 0, 0, 0, time.UTC)
	editCalls, verifyCalls := 0, 0
	business := &managementBusiness{
		edit: func(ctx context.Context, input service.EditKnowledgeDraftInput) (
			*service.KnowledgeDraftEditResult, error,
		) {
			editCalls++
			if ctx.Value(managementContextKey{}) != "request-context" {
				t.Fatal("edit lost request context")
			}
			want := service.EditKnowledgeDraftInput{
				VersionID: 21, ExpectedContentRevision: 3, CategoryID: 7,
				Title: "虚构测试标题", Content: "虚构全文\n保留换行",
				SourceURL: "https://www.bilibili.com/test-only",
				Articles: []service.CreateRuleArticleInput{
					{
						SectionPath: "测试章节", ArticleNumber: "第一条",
						Content: "虚构条款",
					},
				},
			}
			if !reflect.DeepEqual(input, want) {
				t.Fatalf("edit input=%+v want=%+v", input, want)
			}
			return &service.KnowledgeDraftEditResult{
				Version: &model.RuleVersion{ID: 21, ContentRevision: 4},
				Verification: &model.RuleVerification{
					ID: 31, VersionID: 21, VerifiedContentRevision: 3,
					VerifiedBy: 9, VerifiedAt: at, VerificationNote: "旧核验",
					ContentSnapshot: "must-not-leak",
				},
			}, nil
		},
		verify: func(ctx context.Context, id uint64,
			input service.VerifyKnowledgeDraftInput,
		) (*model.RuleVerification, error) {
			verifyCalls++
			if ctx.Value(managementContextKey{}) != "request-context" {
				t.Fatal("verification lost request context")
			}
			want := service.VerifyKnowledgeDraftInput{
				VersionID: 21, ExpectedContentRevision: 3,
				VerificationNote: "虚构测试核验",
			}
			if id != 9 || !reflect.DeepEqual(input, want) {
				t.Fatalf("verifier=%d input=%+v", id, input)
			}
			return &model.RuleVerification{
				ID: 32, VersionID: 21, VerifiedContentRevision: 3,
				VerifiedBy: id, VerifiedAt: at, ContentSnapshot: "must-not-leak",
			}, nil
		},
	}
	r, token := managementRouter(t, model.RoleAdmin, business)

	w := managementRequest(r, "PUT", managementPath, managementEditJSON, token, false)
	managementWantJSON(t, w, 200, map[string]any{
		"version_id": 21, "content_revision": 4,
		"verification": map[string]any{
			"verification_id": 31, "verified_content_revision": 3,
			"verified_by": 9, "verified_at": at,
			"verification_note": "旧核验", "evidence_url": nil,
			"matches_current_content": false,
		},
	})

	// 客户端伪造查询参数和请求头，不能覆盖JWT身份。
	req := httptest.NewRequest(
		"POST", managementPath+"/verifications?verified_by=999&role=admin",
		strings.NewReader(managementVerifyJSON),
	)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-ID", "999")
	req = req.WithContext(context.WithValue(
		req.Context(), managementContextKey{}, "request-context",
	))
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	managementWantJSON(t, w, 201, map[string]any{
		"verification_id": 32, "version_id": 21,
		"verified_content_revision": 3, "verified_by": 9, "verified_at": at,
	})
	if editCalls != 1 || verifyCalls != 1 {
		t.Fatalf("edit calls=%d verification calls=%d", editCalls, verifyCalls)
	}
}

func TestKnowledgeManagementHTTPPermissions(t *testing.T) {
	cases := []struct {
		name, role, code string
		withToken        bool
		status           int
	}{
		{"anonymous", model.RoleAdmin, "unauthorized", false, 401},
		{"ordinary_user", model.RoleUser, "forbidden", true, 403},
		{"support", model.RoleSupport, "forbidden", true, 403},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, token := managementRouter(t, tc.role, &managementBusiness{})
			if !tc.withToken {
				token = ""
			}
			for _, method := range []string{"PUT", "POST"} {
				path, body := managementPath, managementEditJSON
				if method == "POST" {
					path += "/verifications"
					body = managementVerifyJSON
				}
				w := managementRequest(r, method, path, body, token, false)
				managementWantError(t, w, tc.status, tc.code)
				if tc.status == 401 &&
					w.Header().Get("WWW-Authenticate") != "Bearer" {
					t.Fatal("401 response missing Bearer challenge")
				}
			}
		})
	}
}

func TestKnowledgeManagementHTTPRejectsMalformedRequests(t *testing.T) {
	for _, method := range []string{"PUT", "POST"} {
		t.Run(method, func(t *testing.T) {
			r, token := managementRouter(t, model.RoleAdmin, &managementBusiness{})
			path, valid := managementPath, managementEditJSON
			if method == "POST" {
				path += "/verifications"
				valid = managementVerifyJSON
			}
			bodies := []string{
				"", "null", "{", "[]", valid + "{}",
				strings.Replace(valid, `"expected_content_revision":3`,
					`"expected_content_revision":18446744073709551616`, 1),
				strings.Replace(valid, `"expected_content_revision":3`,
					`"expected_content_revision":-1`, 1),
			}
			for _, field := range []string{
				`"verified_by":999`, `"verified_at":"2026-10-27T00:00:00Z"`,
				`"content_snapshot":{}`, `"status":"published"`,
				`"content_revision":4`, `"version_id":999`,
			} {
				bodies = append(bodies, strings.Replace(valid, "{", "{"+field+",", 1))
			}
			if method == "PUT" {
				bodies = append(bodies, strings.Replace(
					valid, `"effective_at":null`, `"effective_at":"not-a-time"`, 1,
				))
			}
			for i, body := range bodies {
				t.Run(fmt.Sprintf("body_%d", i), func(t *testing.T) {
					w := managementRequest(r, method, path, body, token, false)
					managementWantError(t, w, 400, "invalid_argument")
				})
			}
			for _, id := range []string{"abc", "0", "-1", "18446744073709551616"} {
				badPath := strings.Replace(path, "/21", "/"+id, 1)
				w := managementRequest(r, method, badPath, valid, token, false)
				managementWantError(t, w, 400, "invalid_argument")
			}
		})
	}
}

func TestKnowledgeManagementHTTPErrorMapping(t *testing.T) {
	cases := []struct {
		err     error
		status  int
		code    string
		message string
	}{
		{service.ErrInvalidArgument, 400, "invalid_argument", "invalid request"},
		{service.ErrKnowledgeVersionNotFound, 404,
			"knowledge_version_not_found", "knowledge version not found"},
		{service.ErrKnowledgeVersionNotEditable, 409,
			"knowledge_version_not_editable", "knowledge version is not editable"},
		{service.ErrKnowledgeContentConflict, 409,
			"knowledge_content_conflict", "knowledge content revision conflict"},
		{errors.New("SQL secret password=private"), 500,
			"internal_error", "internal server error"},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			calls := 0
			wrapped := fmt.Errorf("outer: %w", tc.err)
			business := &managementBusiness{
				edit: func(context.Context, service.EditKnowledgeDraftInput) (
					*service.KnowledgeDraftEditResult, error,
				) {
					calls++
					return nil, wrapped
				},
				verify: func(context.Context, uint64, service.VerifyKnowledgeDraftInput) (
					*model.RuleVerification, error,
				) {
					calls++
					return nil, wrapped
				},
			}
			r, token := managementRouter(t, model.RoleAdmin, business)
			for _, method := range []string{"PUT", "POST"} {
				path, body := managementPath, managementEditJSON
				if method == "POST" {
					path += "/verifications"
					body = managementVerifyJSON
				}
				w := managementRequest(r, method, path, body, token, false)
				managementWantJSON(t, w, tc.status, map[string]string{
					"code": tc.code, "message": tc.message,
				})
			}
			if calls != 2 {
				t.Fatalf("service calls=%d want=2", calls)
			}
		})
	}
}

func TestKnowledgeManagementHTTPBodyLimit(t *testing.T) {
	for _, method := range []string{"PUT", "POST"} {
		for _, unknownLength := range []bool{false, true} {
			for _, size := range []int{65536, 65537} {
				t.Run(fmt.Sprintf("%s_%v_%d", method, unknownLength, size),
					func(t *testing.T) {
						calls := 0
						business := &managementBusiness{
							edit: func(context.Context, service.EditKnowledgeDraftInput) (
								*service.KnowledgeDraftEditResult, error,
							) {
								calls++
								return &service.KnowledgeDraftEditResult{
									Version: &model.RuleVersion{ID: 21, ContentRevision: 4},
								}, nil
							},
							verify: func(context.Context, uint64, service.VerifyKnowledgeDraftInput) (
								*model.RuleVerification, error,
							) {
								calls++
								return &model.RuleVerification{
									ID: 31, VersionID: 21, VerifiedContentRevision: 3, VerifiedBy: 9,
								}, nil
							},
						}
						r, token := managementRouter(t, model.RoleAdmin, business)
						path, body, success := managementPath, managementEditJSON, 200
						if method == "POST" {
							path += "/verifications"
							body, success = managementVerifyJSON, 201
						}
						body += strings.Repeat(" ", size-len(body))
						w := managementRequest(r, method, path, body, token, unknownLength)
						if size == 65536 {
							if w.Code != success || calls != 1 {
								t.Fatalf("boundary status=%d calls=%d body=%s",
									w.Code, calls, w.Body.String())
							}
						} else {
							managementWantError(t, w, 413, "request_too_large")
							if calls != 0 {
								t.Fatal("oversized request reached service")
							}
						}
					})
			}
		}
	}
}
