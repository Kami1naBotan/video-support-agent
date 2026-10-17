package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"video-support-agent/internal/auth"
	"video-support-agent/internal/model"
	"video-support-agent/internal/service"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

type loginUserStoreStub struct {
	user    *model.User
	findErr error
}

func (s *loginUserStoreStub) FindByUsername(
	_ context.Context,
	username string,
) (*model.User, error) {
	if s.findErr != nil {
		return nil, s.findErr
	}
	if s.user == nil || s.user.Username != username {
		return nil, gorm.ErrRecordNotFound
	}
	return s.user, nil
}

func (s *loginUserStoreStub) Create(
	_ context.Context,
	_ *model.User,
) error {
	return errors.New("login must not create a user")
}

func TestAuthHandlerLogin(t *testing.T) {
	gin.SetMode(gin.TestMode)

	hash, err := bcrypt.GenerateFromPassword(
		[]byte("password123"),
		bcrypt.MinCost,
	)
	if err != nil {
		t.Fatal(err)
	}

	user := &model.User{
		ID:           7,
		Username:     "alice",
		PasswordHash: string(hash),
		Role:         "user",
	}

	tokens, err := auth.NewTokenManager(
		"0123456789abcdef0123456789abcdef",
		"test-support",
		time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		body       string
		findErr    error
		wantStatus int
		wantCode   string
	}{
		{
			name:       "success",
			body:       `{"username":"alice","password":"password123"}`,
			wantStatus: http.StatusOK,
		},
		{
			name:       "wrong password",
			body:       `{"username":"alice","password":"wrongpass123"}`,
			wantStatus: http.StatusUnauthorized,
			wantCode:   "invalid_credentials",
		},
		{
			name:       "unknown user",
			body:       `{"username":"unknown","password":"password123"}`,
			wantStatus: http.StatusUnauthorized,
			wantCode:   "invalid_credentials",
		},
		{
			name:       "invalid JSON",
			body:       `{"username":`,
			wantStatus: http.StatusBadRequest,
			wantCode:   "invalid_argument",
		},
		{
			name:       "database failure",
			body:       `{"username":"alice","password":"password123"}`,
			findErr:    errors.New("database unavailable"),
			wantStatus: http.StatusInternalServerError,
			wantCode:   "internal_error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &loginUserStoreStub{
				user:    user,
				findErr: tt.findErr,
			}
			h := NewAuthHandler(
				service.NewAuthService(store),
				tokens,
			)

			r := gin.New()
			r.POST("/api/v1/auth/login", h.Login)

			request := httptest.NewRequest(
				http.MethodPost,
				"/api/v1/auth/login",
				strings.NewReader(tt.body),
			)
			request.Header.Set("Content-Type", "application/json")

			recorder := httptest.NewRecorder()
			r.ServeHTTP(recorder, request)

			if recorder.Code != tt.wantStatus {
				t.Fatalf(
					"expected status %d, got %d",
					tt.wantStatus,
					recorder.Code,
				)
			}

			var response map[string]json.RawMessage
			if err := json.Unmarshal(
				recorder.Body.Bytes(),
				&response,
			); err != nil {
				t.Fatal(err)
			}

			if _, exists := response["password"]; exists {
				t.Fatal("response exposes password")
			}
			if _, exists := response["password_hash"]; exists {
				t.Fatal("response exposes password hash")
			}

			if tt.wantStatus != http.StatusOK {
				if _, exists := response["access_token"]; exists {
					t.Fatal("failed login must not return a token")
				}

				var code string
				if err := json.Unmarshal(response["code"], &code); err != nil {
					t.Fatal(err)
				}
				if code != tt.wantCode {
					t.Fatalf("expected code %q, got %q", tt.wantCode, code)
				}
				return
			}

			if recorder.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("login response must disable caching")
			}

			var rawToken string
			if err := json.Unmarshal(
				response["access_token"],
				&rawToken,
			); err != nil {
				t.Fatal(err)
			}

			userID, err := tokens.Parse(rawToken)
			if err != nil {
				t.Fatalf("returned token is invalid: %v", err)
			}
			if userID != user.ID {
				t.Fatalf("expected user ID %d, got %d", user.ID, userID)
			}

			var tokenType string
			if err := json.Unmarshal(
				response["token_type"],
				&tokenType,
			); err != nil {
				t.Fatal(err)
			}
			if tokenType != "Bearer" {
				t.Fatalf("unexpected token type: %q", tokenType)
			}
		})
	}
}
