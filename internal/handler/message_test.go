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

type testMessageStore struct {
	created  *model.Message
	messages []model.Message
	err      error
}

func (f *testMessageStore) Create(
	_ context.Context,
	message *model.Message,
) error {
	message.ID = 10
	f.created = message
	return f.err
}

func (f *testMessageStore) ListByConversationID(
	_ context.Context,
	_ uint64,
) ([]model.Message, error) {
	return f.messages, f.err
}

func TestMessageHandlerCreate(t *testing.T) {
	gin.SetMode(gin.TestMode)

	conversations := &testConversationStore{
		conversation: &model.Conversation{ID: 1},
	}
	messages := &testMessageStore{}

	messageService := service.NewMessageService(
		messages,
		conversations,
	)
	handler := NewMessageHandler(messageService)

	router := gin.New()
	router.POST(
		"/api/v1/conversations/:id/messages",
		handler.Create,
	)

	request := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/conversations/1/messages",
		strings.NewReader(`{"content":"账号被封禁后如何申诉？"}`),
	)
	request.Header.Set("Content-Type", "application/json")

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusCreated {
		t.Fatalf("expected status 201, got %d", recorder.Code)
	}

	if !strings.Contains(
		recorder.Body.String(),
		`"content":"账号被封禁后如何申诉？"`,
	) {
		t.Fatalf("unexpected response: %s", recorder.Body.String())
	}

	if messages.created.Role != "user" {
		t.Fatalf("expected role user, got %q", messages.created.Role)
	}
}

func TestMessageHandlerCreateInvalidContent(t *testing.T) {
	gin.SetMode(gin.TestMode)

	conversations := &testConversationStore{
		conversation: &model.Conversation{ID: 1},
	}
	messages := &testMessageStore{}

	handler := NewMessageHandler(
		service.NewMessageService(messages, conversations),
	)

	router := gin.New()
	router.POST(
		"/api/v1/conversations/:id/messages",
		handler.Create,
	)

	request := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/conversations/1/messages",
		strings.NewReader(`{"content":"   "}`),
	)
	request.Header.Set("Content-Type", "application/json")

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d", recorder.Code)
	}

	if messages.created != nil {
		t.Fatal("message should not be created")
	}
}

func TestMessageHandlerCreateInvalidID(t *testing.T) {
	gin.SetMode(gin.TestMode)

	handler := NewMessageHandler(
		service.NewMessageService(
			&testMessageStore{},
			&testConversationStore{},
		),
	)

	router := gin.New()
	router.POST(
		"/api/v1/conversations/:id/messages",
		handler.Create,
	)

	request := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/conversations/not-a-number/messages",
		strings.NewReader(`{"content":"测试"}`),
	)
	request.Header.Set("Content-Type", "application/json")

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d", recorder.Code)
	}
}

func TestMessageHandlerList(t *testing.T) {
	gin.SetMode(gin.TestMode)

	conversations := &testConversationStore{
		conversation: &model.Conversation{ID: 1},
	}
	messages := &testMessageStore{
		messages: []model.Message{
			{
				ID:             1,
				ConversationID: 1,
				Role:           "user",
				Content:        "第一条",
			},
			{
				ID:             2,
				ConversationID: 1,
				Role:           "user",
				Content:        "第二条",
			},
		},
	}

	handler := NewMessageHandler(
		service.NewMessageService(messages, conversations),
	)

	router := gin.New()
	router.GET(
		"/api/v1/conversations/:id/messages",
		handler.List,
	)

	request := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/conversations/1/messages",
		nil,
	)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", recorder.Code)
	}

	body := recorder.Body.String()
	if !strings.Contains(body, `"content":"第一条"`) ||
		!strings.Contains(body, `"content":"第二条"`) {
		t.Fatalf("unexpected response: %s", body)
	}
}

func TestMessageHandlerListConversationNotFound(t *testing.T) {
	gin.SetMode(gin.TestMode)

	conversations := &testConversationStore{
		err: service.ErrConversationNotFound,
	}
	messages := &testMessageStore{}

	handler := NewMessageHandler(
		service.NewMessageService(messages, conversations),
	)

	router := gin.New()
	router.GET(
		"/api/v1/conversations/:id/messages",
		handler.List,
	)

	request := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/conversations/999/messages",
		nil,
	)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("expected status 404, got %d", recorder.Code)
	}
}
