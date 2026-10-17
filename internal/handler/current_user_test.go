package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"video-support-agent/internal/auth"
	"video-support-agent/internal/middleware"
	"video-support-agent/internal/model"
	"video-support-agent/internal/service"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type currentUserStoreStub struct {
	user   *model.User
	err    error
	called bool
	gotID  uint64
}

func (s *currentUserStoreStub) FindByID(
	_ context.Context,
	id uint64,
) (*model.User, error) {
	s.called = true
	s.gotID = id

	if s.err != nil {
		return nil, s.err
	}
	return s.user, nil
}

func TestCurrentUserHandlerMe(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tokens, err := auth.NewTokenManager(
		"0123456789abcdef0123456789abcdef",
		"test-support",
		time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}

	rawToken, err := tokens.Issue(7)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		header     string
		storeErr   error
		wantStatus int
		wantLookup bool
		wantCode   string
	}{
		{
			name:       "authenticated user",
			header:     "Bearer " + rawToken,
			wantStatus: http.StatusOK,
			wantLookup: true,
		},
		{
			name:       "missing token",
			wantStatus: http.StatusUnauthorized,
			wantCode:   "unauthorized",
		},
		{
			name:       "invalid token",
			header:     "Bearer invalid-token",
			wantStatus: http.StatusUnauthorized,
			wantCode:   "unauthorized",
		},
		{
			name:       "user no longer exists",
			header:     "Bearer " + rawToken,
			storeErr:   gorm.ErrRecordNotFound,
			wantStatus: http.StatusUnauthorized,
			wantLookup: true,
			wantCode:   "unauthorized",
		},
		{
			name:       "database failure",
			header:     "Bearer " + rawToken,
			storeErr:   errors.New("database unavailable"),
			wantStatus: http.StatusInternalServerError,
			wantLookup: true,
			wantCode:   "internal_error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &currentUserStoreStub{
				user: &model.User{
					ID:           7,
					Username:     "alice",
					Role:         "support",
					PasswordHash: "must-not-be-exposed",
				},
				err: tt.storeErr,
			}

			h := NewCurrentUserHandler(
				service.NewCurrentUserService(store),
			)

			r := gin.New()
			r.GET("/api/v1/me", middleware.JWTAuth(tokens), h.Me)

			// 故意携带其他用户 ID，验证接口仍使用 Token 中的身份。
			request := httptest.NewRequest(
				http.MethodGet,
				"/api/v1/me?user_id=999",
				nil,
			)
			if tt.header != "" {
				request.Header.Set("Authorization", tt.header)
			}

			recorder := httptest.NewRecorder()
			r.ServeHTTP(recorder, request)

			if recorder.Code != tt.wantStatus {
				t.Fatalf(
					"expected status %d, got %d",
					tt.wantStatus,
					recorder.Code,
				)
			}

			if store.called != tt.wantLookup {
				t.Fatalf("unexpected database lookup: %v", store.called)
			}
			if store.called && store.gotID != 7 {
				t.Fatalf("expected lookup of user 7, got %d", store.gotID)
			}

			if tt.wantStatus != http.StatusOK {
				var response ErrorResponse
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
				return
			}

			var fields map[string]json.RawMessage
			if err := json.Unmarshal(
				recorder.Body.Bytes(),
				&fields,
			); err != nil {
				t.Fatal(err)
			}
			if len(fields) != 3 ||
				fields["id"] == nil ||
				fields["username"] == nil ||
				fields["role"] == nil {
				t.Fatal("response must contain only id, username and role")
			}

			var response struct {
				ID       uint64 `json:"id"`
				Username string `json:"username"`
				Role     string `json:"role"`
			}
			if err := json.Unmarshal(
				recorder.Body.Bytes(),
				&response,
			); err != nil {
				t.Fatal(err)
			}

			if response.ID != 7 ||
				response.Username != "alice" ||
				response.Role != "support" {
				t.Fatalf("unexpected current user: %+v", response)
			}
		})
	}
}
