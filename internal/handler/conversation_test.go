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
)

type testUserStore struct {
	exists bool
	err    error
}

func (f *testUserStore) Exists(context.Context, uint64) (bool, error) {
	return f.exists, f.err
}

type testConversationStore struct {
	conversation *model.Conversation
	err          error
}

func (f *testConversationStore) Create(
	_ context.Context,
	conversation *model.Conversation,
) error {
	conversation.ID = 1
	f.conversation = conversation
	return nil
}

func (f *testConversationStore) GetByID(
	_ context.Context,
	_ uint64,
) (*model.Conversation, error) {
	return f.conversation, f.err
}

func TestConversationHandlerCreate(t *testing.T) {
	gin.SetMode(gin.TestMode)

	conversations := &testConversationStore{}
	users := &testUserStore{exists: true}
	conversationService := service.NewConversationService(
		conversations,
		users,
	)
	handler := NewConversationHandler(conversationService)

	router := gin.New()
	router.POST(
		"/api/v1/conversations",
		handler.Create,
	)

	request := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/conversations",
		strings.NewReader(`{"user_id":7,"title":"账号申诉咨询"}`),
	)
	request.Header.Set("Content-Type", "application/json")

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusCreated {
		t.Fatalf("expected status 201, got %d", recorder.Code)
	}

	if !strings.Contains(recorder.Body.String(), `"title":"账号申诉咨询"`) {
		t.Fatalf("unexpected response: %s", recorder.Body.String())
	}
}

func TestConversationHandlerCreateInvalidJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)

	users := &testUserStore{exists: true}
	conversations := &testConversationStore{}
	handler := NewConversationHandler(
		service.NewConversationService(conversations, users),
	)

	router := gin.New()
	router.POST("/api/v1/conversations", handler.Create)

	request := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/conversations",
		strings.NewReader(`{"user_id":7`),
	)
	request.Header.Set("Content-Type", "application/json")

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d", recorder.Code)
	}

	expected := `{"code":"invalid_argument","message":"invalid request"}`
	if recorder.Body.String() != expected {
		t.Fatalf("expected body %s, got %s", expected, recorder.Body.String())
	}
}

func TestConversationHandlerCreateUserNotFound(t *testing.T) {
	gin.SetMode(gin.TestMode)

	users := &testUserStore{exists: false}
	conversations := &testConversationStore{}
	handler := NewConversationHandler(
		service.NewConversationService(conversations, users),
	)

	router := gin.New()
	router.POST("/api/v1/conversations", handler.Create)

	request := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/conversations",
		strings.NewReader(`{"user_id":999,"title":"测试"}`),
	)
	request.Header.Set("Content-Type", "application/json")

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("expected status 404, got %d", recorder.Code)
	}
}

func TestConversationHandlerGetByID(t *testing.T) {
	gin.SetMode(gin.TestMode)

	conversations := &testConversationStore{
		conversation: &model.Conversation{
			ID:     1,
			UserID: 7,
			Title:  "账号申诉咨询",
		},
	}
	users := &testUserStore{}
	handler := NewConversationHandler(
		service.NewConversationService(conversations, users),
	)

	router := gin.New()
	router.GET("/api/v1/conversations/:id", handler.GetByID)

	request := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/conversations/1",
		nil,
	)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", recorder.Code)
	}
}

func TestConversationHandlerGetByIDNotFound(t *testing.T) {
	gin.SetMode(gin.TestMode)

	conversations := &testConversationStore{
		err: service.ErrConversationNotFound,
	}
	handler := NewConversationHandler(
		service.NewConversationService(
			conversations,
			&testUserStore{},
		),
	)

	router := gin.New()
	router.GET("/api/v1/conversations/:id", handler.GetByID)

	request := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/conversations/999",
		nil,
	)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("expected status 404, got %d", recorder.Code)
	}
}
