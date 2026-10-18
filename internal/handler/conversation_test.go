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

func (f *testConversationStore) ListByUserID(
	_ context.Context,
	userID uint64,
) ([]model.Conversation, error) {
	if f.err != nil {
		return nil, f.err
	}

	if f.conversation == nil || f.conversation.UserID != userID {
		return []model.Conversation{}, nil
	}

	return []model.Conversation{*f.conversation}, nil
}

func TestConversationHandlerCreateAuthenticated(t *testing.T) {
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
		body       string
		withToken  bool
		userExists bool
		wantStatus int
	}{
		{
			name:       "create own conversation",
			body:       `{"title":"账号申诉咨询"}`,
			withToken:  true,
			userExists: true,
			wantStatus: http.StatusCreated,
		},
		{
			name:       "ignore forged user ID",
			body:       `{"user_id":999,"title":"账号申诉咨询"}`,
			withToken:  true,
			userExists: true,
			wantStatus: http.StatusCreated,
		},
		{
			name:       "missing token",
			body:       `{"title":"账号申诉咨询"}`,
			userExists: true,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "invalid JSON",
			body:       `{"title":`,
			withToken:  true,
			userExists: true,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "empty title",
			body:       `{"title":""}`,
			withToken:  true,
			userExists: true,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "user no longer exists",
			body:       `{"title":"账号申诉咨询"}`,
			withToken:  true,
			userExists: false,
			wantStatus: http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conversations := &testConversationStore{}
			users := &testUserStore{exists: tt.userExists}

			h := NewConversationHandler(
				service.NewConversationService(conversations, users),
			)

			router := gin.New()
			router.POST(
				"/api/v1/conversations",
				middleware.JWTAuth(tokens),
				h.Create,
			)

			request := httptest.NewRequest(
				http.MethodPost,
				"/api/v1/conversations",
				strings.NewReader(tt.body),
			)
			request.Header.Set("Content-Type", "application/json")
			if tt.withToken {
				request.Header.Set(
					"Authorization",
					"Bearer "+rawToken,
				)
			}

			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)

			if recorder.Code != tt.wantStatus {
				t.Fatalf(
					"expected status %d, got %d; response: %s",
					tt.wantStatus,
					recorder.Code,
					recorder.Body.String(),
				)
			}

			if tt.wantStatus != http.StatusCreated {
				if conversations.conversation != nil {
					t.Fatal("failed request must not create a conversation")
				}
				return
			}

			saved := conversations.conversation
			if saved == nil {
				t.Fatal("expected conversation to be saved")
			}
			if saved.UserID != 7 {
				t.Fatalf(
					"expected owner from JWT to be 7, got %d",
					saved.UserID,
				)
			}
			if saved.Title != "账号申诉咨询" {
				t.Fatalf("unexpected title: %q", saved.Title)
			}
		})
	}
}

func TestConversationHandlerGetByIDOwnership(t *testing.T) {
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
		path       string
		withToken  bool
		ownerID    uint64
		storeErr   error
		wantStatus int
	}{
		{
			name:       "own conversation",
			path:       "/api/v1/conversations/1",
			withToken:  true,
			ownerID:    7,
			wantStatus: http.StatusOK,
		},
		{
			name:       "cannot impersonate owner with query parameter",
			path:       "/api/v1/conversations/1?user_id=8",
			withToken:  true,
			ownerID:    8,
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "missing conversation",
			path:       "/api/v1/conversations/1",
			withToken:  true,
			storeErr:   service.ErrConversationNotFound,
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "missing token",
			path:       "/api/v1/conversations/1",
			ownerID:    7,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "invalid path ID",
			path:       "/api/v1/conversations/abc",
			withToken:  true,
			ownerID:    7,
			wantStatus: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &testConversationStore{
				conversation: &model.Conversation{
					ID:     1,
					UserID: tt.ownerID,
					Title:  "private-conversation-title",
				},
				err: tt.storeErr,
			}

			h := NewConversationHandler(
				service.NewConversationService(store, &testUserStore{}),
			)

			router := gin.New()
			router.GET(
				"/api/v1/conversations/:id",
				middleware.JWTAuth(tokens),
				h.GetByID,
			)

			request := httptest.NewRequest(
				http.MethodGet,
				tt.path,
				nil,
			)
			if tt.withToken {
				request.Header.Set("Authorization", "Bearer "+rawToken)
			}

			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)

			if recorder.Code != tt.wantStatus {
				t.Fatalf(
					"expected status %d, got %d; response: %s",
					tt.wantStatus,
					recorder.Code,
					recorder.Body.String(),
				)
			}

			hasTitle := strings.Contains(
				recorder.Body.String(),
				"private-conversation-title",
			)
			if tt.wantStatus == http.StatusOK && !hasTitle {
				t.Fatal("owner should receive conversation data")
			}
			if tt.wantStatus != http.StatusOK && hasTitle {
				t.Fatal("failed request must not expose conversation data")
			}
		})
	}
}
