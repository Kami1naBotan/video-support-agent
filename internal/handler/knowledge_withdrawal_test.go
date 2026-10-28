package handler_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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

const withdrawalHTTPPath = "/api/v1/admin/knowledge/documents/11/withdraw"
const withdrawalHTTPJSON = `{"version_id":21,"expected_publication_revision":7}`

type withdrawalBusiness struct {
	managementBusiness

	withdraw func(
		context.Context,
		uint64,
		service.WithdrawKnowledgeVersionInput,
	) (*service.KnowledgeWithdrawalResult, error)
}

var _ handler.KnowledgeService = (*withdrawalBusiness)(nil)
var _ handler.KnowledgeWithdrawalService = (*withdrawalBusiness)(nil)

func (b *withdrawalBusiness) WithdrawVersion(
	ctx context.Context,
	operatorID uint64,
	input service.WithdrawKnowledgeVersionInput,
) (*service.KnowledgeWithdrawalResult, error) {
	if b.withdraw == nil {
		panic("request must not reach withdrawal service")
	}
	return b.withdraw(ctx, operatorID, input)
}

// 真实JWT、角色检查和Handler；用户读取及业务服务使用替身。
// 实际router.New留板块4验证。
func withdrawalHTTPRouter(
	t *testing.T,
	role string,
	business handler.KnowledgeService,
) (*gin.Engine, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	tokens, err := auth.NewTokenManager(
		"0123456789abcdef0123456789abcdef",
		"knowledge-withdrawal-http-test",
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
		middleware.RequireRoles(
			managementUserReader{role: role},
			model.RoleAdmin,
		),
	)
	h := handler.NewKnowledgeHandler(business)
	group.POST("/documents/:document_id/withdraw", h.Withdraw)
	return r, token
}

func TestKnowledgeWithdrawalHTTPSuccess(t *testing.T) {
	for _, revision := range []uint64{0, 7} {
		t.Run(fmt.Sprintf("revision_%d", revision), func(t *testing.T) {
			at := time.Date(2026, 10, 28, 2, 3, 4, 123000000, time.UTC)
			calls := 0
			business := &withdrawalBusiness{
				withdraw: func(
					ctx context.Context,
					operatorID uint64,
					input service.WithdrawKnowledgeVersionInput,
				) (*service.KnowledgeWithdrawalResult, error) {
					calls++
					if ctx.Value(managementContextKey{}) != "request-context" {
						t.Fatal("request context was not forwarded")
					}
					want := service.WithdrawKnowledgeVersionInput{
						DocumentID:                  11,
						VersionID:                   21,
						ExpectedPublicationRevision: revision,
					}
					if operatorID != 9 || input != want {
						t.Fatalf("operator=%d input=%+v, want 9 and %+v",
							operatorID, input, want)
					}
					return &service.KnowledgeWithdrawalResult{
						DocumentID:          11,
						VersionID:           21,
						PublicationRevision: revision + 1,
						WithdrawnAt:         at,
					}, nil
				},
			}
			r, token := withdrawalHTTPRouter(t, model.RoleAdmin, business)
			body := fmt.Sprintf(
				`{"version_id":21,"expected_publication_revision":%d}`,
				revision,
			)
			w := managementRequest(
				r, http.MethodPost, withdrawalHTTPPath, body, token, false,
			)

			// 完整JSON比较同时检查没有额外管理字段。
			managementWantJSON(t, w, http.StatusOK, map[string]any{
				"document_id":          11,
				"version_id":           21,
				"publication_revision": revision + 1,
				"withdrawn_at":         at,
			})
			if calls != 1 {
				t.Fatalf("service calls=%d, want 1", calls)
			}
		})
	}
}

func TestKnowledgeWithdrawalHTTPInvalidInput(t *testing.T) {
	tests := []struct {
		name string
		id   string
		body string
	}{
		{"zero_document", "0", withdrawalHTTPJSON},
		{"negative_document", "-1", withdrawalHTTPJSON},
		{"text_document", "abc", withdrawalHTTPJSON},
		{"overflow_document", "18446744073709551616", withdrawalHTTPJSON},
		{"empty_body", "11", ""},
		{"null_body", "11", "null"},
		{"array_body", "11", "[]"},
		{"empty_object", "11", "{}"},
		{"malformed_json", "11", `{"version_id":`},
		{"multiple_json", "11", withdrawalHTTPJSON + `{}`},
		{"trailing_garbage", "11", withdrawalHTTPJSON + `x`},
		{"missing_version", "11", `{"expected_publication_revision":7}`},
		{"null_version", "11", `{"version_id":null,"expected_publication_revision":7}`},
		{"zero_version", "11", `{"version_id":0,"expected_publication_revision":7}`},
		{"negative_version", "11", `{"version_id":-1,"expected_publication_revision":7}`},
		{"string_version", "11", `{"version_id":"21","expected_publication_revision":7}`},
		{"overflow_version", "11", `{"version_id":18446744073709551616,"expected_publication_revision":7}`},
		{"missing_revision", "11", `{"version_id":21}`},
		{"null_revision", "11", `{"version_id":21,"expected_publication_revision":null}`},
		{"negative_revision", "11", `{"version_id":21,"expected_publication_revision":-1}`},
		{"fraction_revision", "11", `{"version_id":21,"expected_publication_revision":1.5}`},
		{"string_revision", "11", `{"version_id":21,"expected_publication_revision":"7"}`},
		{"overflow_revision", "11", `{"version_id":21,"expected_publication_revision":18446744073709551616}`},
		{"forged_operator", "11", `{"version_id":21,"expected_publication_revision":7,"operator_id":99}`},
		{"forged_document", "11", `{"version_id":21,"expected_publication_revision":7,"document_id":99}`},
		{"forged_status", "11", `{"version_id":21,"expected_publication_revision":7,"status":"withdrawn"}`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// 未配置withdraw；如果非法请求进入业务层，测试直接失败。
			r, token := withdrawalHTTPRouter(
				t, model.RoleAdmin, &withdrawalBusiness{},
			)
			path := "/api/v1/admin/knowledge/documents/" + tc.id + "/withdraw"
			w := managementRequest(
				r, http.MethodPost, path, tc.body, token, false,
			)
			managementWantJSON(t, w, http.StatusBadRequest, map[string]any{
				"code":    "invalid_argument",
				"message": "invalid request",
			})
		})
	}
}

func TestKnowledgeWithdrawalHTTPAuthorization(t *testing.T) {
	tests := []struct {
		name   string
		role   string
		token  string
		status int
		code   string
	}{
		{"anonymous", model.RoleAdmin, "none", 401, "unauthorized"},
		{"invalid_token", model.RoleAdmin, "invalid", 401, "unauthorized"},
		{"user", model.RoleUser, "valid", 403, "forbidden"},
		{"support", model.RoleSupport, "valid", 403, "forbidden"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, token := withdrawalHTTPRouter(t, tc.role, &withdrawalBusiness{})
			switch tc.token {
			case "none":
				token = ""
			case "invalid":
				token = "invalid-token"
			}

			w := managementRequest(
				r, http.MethodPost, withdrawalHTTPPath,
				withdrawalHTTPJSON, token, false,
			)
			managementWantError(t, w, tc.status, tc.code)
			if tc.status == http.StatusUnauthorized &&
				w.Header().Get("WWW-Authenticate") != "Bearer" {
				t.Fatalf("missing Bearer challenge: %v", w.Header())
			}
		})
	}
}

func TestKnowledgeWithdrawalHTTPErrorMapping(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		status  int
		code    string
		message string
	}{
		{
			"document_not_found", service.ErrKnowledgeDocumentNotFound,
			404, "knowledge_document_not_found", "knowledge document not found",
		},
		{
			"version_not_found_or_wrong_owner", service.ErrKnowledgeVersionNotFound,
			404, "knowledge_version_not_found", "knowledge version not found",
		},
		{
			"stale_revision", service.ErrKnowledgePublicationConflict,
			409, "knowledge_publication_conflict",
			"knowledge publication revision conflict",
		},
		{
			"noncurrent_or_already_withdrawn", service.ErrKnowledgeVersionNotWithdrawable,
			409, "knowledge_version_not_withdrawable",
			"knowledge version is not withdrawable",
		},
		{
			"unauthorized", service.ErrUnauthorized,
			401, "unauthorized", "authentication required",
		},
		{
			"invalid_argument", service.ErrInvalidArgument,
			400, "invalid_argument", "invalid request",
		},
		{
			"deadline", context.DeadlineExceeded,
			504, "request_timeout", "request timed out",
		},
		{
			"internal", errors.New("test-only SQL/password detail"),
			500, "internal_error", "internal server error",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			business := &withdrawalBusiness{
				withdraw: func(
					context.Context,
					uint64,
					service.WithdrawKnowledgeVersionInput,
				) (*service.KnowledgeWithdrawalResult, error) {
					calls++
					return nil, fmt.Errorf("private storage context: %w", tc.err)
				},
			}
			r, token := withdrawalHTTPRouter(t, model.RoleAdmin, business)
			w := managementRequest(
				r, http.MethodPost, withdrawalHTTPPath,
				withdrawalHTTPJSON, token, false,
			)
			managementWantJSON(t, w, tc.status, map[string]any{
				"code":    tc.code,
				"message": tc.message,
			})
			if calls != 1 {
				t.Fatalf("service calls=%d, want 1", calls)
			}
			if tc.status == http.StatusUnauthorized &&
				w.Header().Get("WWW-Authenticate") != "Bearer" {
				t.Fatal("service unauthorized response lacks Bearer challenge")
			}
		})
	}
}

func TestKnowledgeWithdrawalHTTPInternalConfiguration(t *testing.T) {
	t.Run("missing_capability", func(t *testing.T) {
		r, token := withdrawalHTTPRouter(
			t, model.RoleAdmin, &managementBusiness{},
		)
		w := managementRequest(
			r, http.MethodPost, withdrawalHTTPPath,
			withdrawalHTTPJSON, token, false,
		)
		managementWantJSON(t, w, 500, map[string]any{
			"code":    "internal_error",
			"message": "internal server error",
		})
	})

	t.Run("empty_success", func(t *testing.T) {
		calls := 0
		business := &withdrawalBusiness{
			withdraw: func(
				context.Context,
				uint64,
				service.WithdrawKnowledgeVersionInput,
			) (*service.KnowledgeWithdrawalResult, error) {
				calls++
				return nil, nil
			},
		}
		r, token := withdrawalHTTPRouter(t, model.RoleAdmin, business)
		w := managementRequest(
			r, http.MethodPost, withdrawalHTTPPath,
			withdrawalHTTPJSON, token, false,
		)
		managementWantJSON(t, w, 500, map[string]any{
			"code":    "internal_error",
			"message": "internal server error",
		})
		if calls != 1 {
			t.Fatalf("service calls=%d, want 1", calls)
		}
	})
}

func TestKnowledgeWithdrawalHTTPBodyLimit(t *testing.T) {
	for _, unknownLength := range []bool{false, true} {
		for _, size := range []int{65536, 65537} {
			t.Run(fmt.Sprintf("unknown_%t_bytes_%d", unknownLength, size),
				func(t *testing.T) {
					calls := 0
					business := &withdrawalBusiness{
						withdraw: func(
							context.Context,
							uint64,
							service.WithdrawKnowledgeVersionInput,
						) (*service.KnowledgeWithdrawalResult, error) {
							calls++
							return &service.KnowledgeWithdrawalResult{
								DocumentID:          11,
								VersionID:           21,
								PublicationRevision: 8,
								WithdrawnAt: time.Date(
									2026, 10, 28, 2, 0, 0, 0, time.UTC,
								),
							}, nil
						},
					}
					r, token := withdrawalHTTPRouter(t, model.RoleAdmin, business)

					// 合法JSON后补空白，保证测试的是完整请求体字节上限。
					body := withdrawalHTTPJSON +
						strings.Repeat(" ", size-len(withdrawalHTTPJSON))
					w := managementRequest(
						r, http.MethodPost, withdrawalHTTPPath,
						body, token, unknownLength,
					)

					if size == 65536 {
						if w.Code != http.StatusOK || calls != 1 {
							t.Fatalf("status=%d calls=%d body=%s",
								w.Code, calls, w.Body.String())
						}
					} else {
						managementWantJSON(t, w, 413, map[string]any{
							"code":    "request_too_large",
							"message": "request body too large",
						})
						if calls != 0 {
							t.Fatalf("oversized request reached service: %d", calls)
						}
					}
				})
		}
	}
}
