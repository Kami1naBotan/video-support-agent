package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"video-support-agent/internal/model"
	"video-support-agent/internal/service"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type authHandlerUserStoreStub struct {
	user      *model.User
	createErr error
}

func (s *authHandlerUserStoreStub) Create(
	_ context.Context,
	user *model.User,
) error {
	if s.createErr != nil {
		return s.createErr
	}

	user.ID = 7
	s.user = user
	return nil
}

func (s *authHandlerUserStoreStub) FindByUsername(
	_ context.Context,
	_ string,
) (*model.User, error) {
	return nil, gorm.ErrRecordNotFound
}
func TestAuthHandlerRegister(t *testing.T) {
	gin.SetMode(gin.TestMode)

	store := &authHandlerUserStoreStub{}
	authService := service.NewAuthService(store)
	authHandler := NewAuthHandler(authService, nil)

	router := gin.New()
	router.POST("/api/v1/auth/register", authHandler.Register)

	request := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/auth/register",
		strings.NewReader(`{"username":"alice","password":"password123"}`),
	)
	request.Header.Set("Content-Type", "application/json")

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusCreated {
		t.Fatalf("expected status 201, got %d", recorder.Code)
	}

	body := recorder.Body.String()
	if !strings.Contains(body, `"username":"alice"`) {
		t.Fatalf("unexpected response: %s", body)
	}
	if !strings.Contains(body, `"role":"user"`) {
		t.Fatalf("unexpected response: %s", body)
	}
	if strings.Contains(body, "password_hash") {
		t.Fatalf("response must not expose password hash: %s", body)
	}
	if store.user == nil || store.user.PasswordHash == "" {
		t.Fatal("expected password hash to be stored")
	}
}

func TestAuthHandlerRegisterInvalidJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)

	store := &authHandlerUserStoreStub{}
	authHandler := NewAuthHandler(
		service.NewAuthService(store),
		nil,
	)

	router := gin.New()
	router.POST("/api/v1/auth/register", authHandler.Register)

	request := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/auth/register",
		strings.NewReader(`{"username":"alice"`),
	)
	request.Header.Set("Content-Type", "application/json")

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d", recorder.Code)
	}
}

func TestAuthHandlerRegisterDuplicateUsername(t *testing.T) {
	gin.SetMode(gin.TestMode)

	store := &authHandlerUserStoreStub{
		createErr: service.ErrUsernameExists,
	}
	authHandler := NewAuthHandler(
		service.NewAuthService(store),
		nil,
	)

	router := gin.New()
	router.POST("/api/v1/auth/register", authHandler.Register)

	request := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/auth/register",
		strings.NewReader(`{"username":"alice","password":"password123"}`),
	)
	request.Header.Set("Content-Type", "application/json")

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusConflict {
		t.Fatalf("expected status 409, got %d", recorder.Code)
	}
}
