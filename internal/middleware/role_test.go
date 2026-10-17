package middleware

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"video-support-agent/internal/auth"
	"video-support-agent/internal/model"
	"video-support-agent/internal/service"

	"github.com/gin-gonic/gin"
)

type roleUserReaderStub struct {
	role   string
	err    error
	called bool
	gotID  uint64
}

func (s *roleUserReaderStub) Get(
	_ context.Context,
	id uint64,
) (*model.User, error) {
	s.called = true
	s.gotID = id

	if s.err != nil {
		return nil, s.err
	}

	return &model.User{
		ID:   id,
		Role: s.role,
	}, nil
}

func TestRequireRoles(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tokens, err := auth.NewTokenManager(
		"0123456789abcdef0123456789abcdef",
		"test-support",
		time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}

	token, err := tokens.Issue(7)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		role       string
		allowed    []string
		withToken  bool
		readerErr  error
		wantStatus int
		wantCode   string
	}{
		{
			name:       "ordinary user denied",
			role:       model.RoleUser,
			allowed:    []string{model.RoleSupport, model.RoleAdmin},
			withToken:  true,
			wantStatus: http.StatusForbidden,
			wantCode:   "forbidden",
		},
		{
			name:       "support allowed",
			role:       model.RoleSupport,
			allowed:    []string{model.RoleSupport, model.RoleAdmin},
			withToken:  true,
			wantStatus: http.StatusOK,
		},
		{
			name:       "admin allowed",
			role:       model.RoleAdmin,
			allowed:    []string{model.RoleSupport, model.RoleAdmin},
			withToken:  true,
			wantStatus: http.StatusOK,
		},
		{
			name:       "support denied on admin-only route",
			role:       model.RoleSupport,
			allowed:    []string{model.RoleAdmin},
			withToken:  true,
			wantStatus: http.StatusForbidden,
			wantCode:   "forbidden",
		},
		{
			name:       "unknown role denied",
			role:       "superuser",
			allowed:    []string{model.RoleAdmin},
			withToken:  true,
			wantStatus: http.StatusForbidden,
			wantCode:   "forbidden",
		},
		{
			name:       "empty allowlist denies access",
			role:       model.RoleAdmin,
			withToken:  true,
			wantStatus: http.StatusForbidden,
			wantCode:   "forbidden",
		},
		{
			name:       "missing token",
			role:       model.RoleAdmin,
			allowed:    []string{model.RoleAdmin},
			wantStatus: http.StatusUnauthorized,
			wantCode:   "unauthorized",
		},
		{
			name:       "user no longer exists",
			allowed:    []string{model.RoleAdmin},
			withToken:  true,
			readerErr:  service.ErrUnauthorized,
			wantStatus: http.StatusUnauthorized,
			wantCode:   "unauthorized",
		},
		{
			name:       "database failure",
			allowed:    []string{model.RoleAdmin},
			withToken:  true,
			readerErr:  errors.New("database unavailable"),
			wantStatus: http.StatusInternalServerError,
			wantCode:   "internal_error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			users := &roleUserReaderStub{
				role: tt.role,
				err:  tt.readerErr,
			}
			handlerCalled := false

			router := gin.New()
			router.GET(
				"/restricted",
				JWTAuth(tokens),
				RequireRoles(users, tt.allowed...),
				func(c *gin.Context) {
					handlerCalled = true
					c.Status(http.StatusOK)
				},
			)

			// 客户端伪造角色和用户 ID，不能改变授权结果。
			request := httptest.NewRequest(
				http.MethodGet,
				"/restricted?role=admin&user_id=999",
				nil,
			)
			if tt.withToken {
				request.Header.Set("Authorization", "Bearer "+token)
			}

			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)

			if recorder.Code != tt.wantStatus {
				t.Fatalf(
					"expected status %d, got %d",
					tt.wantStatus,
					recorder.Code,
				)
			}

			if users.called != tt.withToken {
				t.Fatalf("unexpected user lookup: %v", users.called)
			}
			if users.called && users.gotID != 7 {
				t.Fatalf("expected JWT user ID 7, got %d", users.gotID)
			}

			wantHandlerCall := tt.wantStatus == http.StatusOK
			if handlerCalled != wantHandlerCall {
				t.Fatalf("unexpected handler call: %v", handlerCalled)
			}

			if tt.wantCode != "" {
				var response struct {
					Code string `json:"code"`
				}
				if err := json.Unmarshal(
					recorder.Body.Bytes(),
					&response,
				); err != nil {
					t.Fatal(err)
				}
				if response.Code != tt.wantCode {
					t.Fatalf(
						"expected code %q, got %q",
						tt.wantCode,
						response.Code,
					)
				}
			}
		})
	}
}

func TestRequireRolesUsesLatestRole(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tokens, err := auth.NewTokenManager(
		"0123456789abcdef0123456789abcdef",
		"test-support",
		time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}

	token, err := tokens.Issue(7)
	if err != nil {
		t.Fatal(err)
	}

	users := &roleUserReaderStub{role: model.RoleAdmin}

	router := gin.New()
	router.GET(
		"/restricted",
		JWTAuth(tokens),
		RequireRoles(users, model.RoleAdmin),
		func(c *gin.Context) {
			c.Status(http.StatusOK)
		},
	)

	sendRequest := func() int {
		request := httptest.NewRequest(http.MethodGet, "/restricted", nil)
		request.Header.Set("Authorization", "Bearer "+token)

		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		return recorder.Code
	}

	if status := sendRequest(); status != http.StatusOK {
		t.Fatalf("expected initial access, got %d", status)
	}

	// 模拟数据库角色变更，继续使用同一个 Token。
	users.role = model.RoleUser

	if status := sendRequest(); status != http.StatusForbidden {
		t.Fatalf("expected access denied after role change, got %d", status)
	}
}
