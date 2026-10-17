package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"video-support-agent/internal/auth"
	"video-support-agent/internal/middleware"
	"video-support-agent/internal/model"
	"video-support-agent/internal/service"

	"github.com/gin-gonic/gin"
)

type testMessageStore struct {
	created  *model.Message
	messages []model.Message
	err      error
	listed   bool
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
	f.listed = true
	return f.messages, f.err
}

func TestMessageHandlerCreate(t *testing.T) {
	gin.SetMode(gin.TestMode)

	conversations := &testConversationStore{
		conversation: &model.Conversation{ID: 1, UserID: 7},
	}
	messages := &testMessageStore{}

	messageService := service.NewMessageService(
		messages,
		conversations,
		nil,
		nil,
	)
	handler := NewMessageHandler(messageService)

	router, token := newMessageTestRouter(t)
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
	request.Header.Set("Authorization", "Bearer "+token)
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
		conversation: &model.Conversation{ID: 1, UserID: 7},
	}
	messages := &testMessageStore{}

	handler := NewMessageHandler(
		service.NewMessageService(messages, conversations, nil, nil),
	)

	router, token := newMessageTestRouter(t)
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
	request.Header.Set("Authorization", "Bearer "+token)
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
			nil,
			nil,
		),
	)

	router, token := newMessageTestRouter(t)
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
	request.Header.Set("Authorization", "Bearer "+token)
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d", recorder.Code)
	}
}

func TestMessageHandlerList(t *testing.T) {
	gin.SetMode(gin.TestMode)

	conversations := &testConversationStore{
		conversation: &model.Conversation{ID: 1, UserID: 7},
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
		service.NewMessageService(messages, conversations, nil, nil),
	)

	router, token := newMessageTestRouter(t)
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
	request.Header.Set("Authorization", "Bearer "+token)
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
		service.NewMessageService(messages, conversations, nil, nil),
	)

	router, token := newMessageTestRouter(t)
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
	request.Header.Set("Authorization", "Bearer "+token)
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("expected status 404, got %d", recorder.Code)
	}
}

func newMessageTestRouter(t *testing.T) (*gin.Engine, string) {
	t.Helper()
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

	router := gin.New()
	router.Use(middleware.JWTAuth(tokens))
	return router, token
}

func TestMessageHandlerAccessDenied(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodGet} {
		t.Run(method, func(t *testing.T) {
			tests := []struct {
				name       string
				withToken  bool
				ownerID    uint64
				wantStatus int
			}{
				{
					name:       "missing token",
					ownerID:    7,
					wantStatus: http.StatusUnauthorized,
				},
				{
					name:       "another user's conversation",
					withToken:  true,
					ownerID:    8,
					wantStatus: http.StatusNotFound,
				},
			}

			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					messages := &testMessageStore{}
					conversations := &testConversationStore{
						conversation: &model.Conversation{
							ID:     1,
							UserID: tt.ownerID,
						},
					}
					h := NewMessageHandler(
						service.NewMessageService(messages, conversations, nil, nil),
					)

					router, token := newMessageTestRouter(t)
					router.POST(
						"/api/v1/conversations/:id/messages",
						h.Create,
					)
					router.GET(
						"/api/v1/conversations/:id/messages",
						h.List,
					)

					request := httptest.NewRequest(
						method,
						"/api/v1/conversations/1/messages?user_id=8",
						strings.NewReader(
							`{"content":"测试","user_id":8}`,
						),
					)
					request.Header.Set("Content-Type", "application/json")
					if tt.withToken {
						request.Header.Set(
							"Authorization",
							"Bearer "+token,
						)
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
					if messages.created != nil || messages.listed {
						t.Fatal("rejected request must not access messages")
					}
				})
			}
		})
	}
}
