package middleware

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"video-support-agent/internal/auth"

	"github.com/gin-gonic/gin"
)

func TestJWTAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)

	manager, err := auth.NewTokenManager(
		"0123456789abcdef0123456789abcdef",
		"test-support",
		time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}

	validToken, err := manager.Issue(7)
	if err != nil {
		t.Fatal(err)
	}

	otherManager, err := auth.NewTokenManager(
		"abcdef0123456789abcdef0123456789ab",
		"test-support",
		time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}

	wrongKeyToken, err := otherManager.Issue(7)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		header     string
		wantStatus int
	}{
		{
			name:       "valid token",
			header:     "Bearer " + validToken,
			wantStatus: http.StatusOK,
		},
		{
			name:       "case insensitive scheme",
			header:     "bearer " + validToken,
			wantStatus: http.StatusOK,
		},
		{
			name:       "missing header",
			header:     "",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "missing token",
			header:     "Bearer",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "wrong scheme",
			header:     "Basic " + validToken,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "malformed token",
			header:     "Bearer not-a-jwt",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "wrong signing key",
			header:     "Bearer " + wrongKeyToken,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "extra header field",
			header:     "Bearer " + validToken + " extra",
			wantStatus: http.StatusUnauthorized,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handlerCalled := false

			router := gin.New()
			router.GET(
				"/private",
				JWTAuth(manager),
				func(c *gin.Context) {
					handlerCalled = true

					userID, ok := CurrentUserID(c)
					if !ok || userID != 7 {
						t.Error("expected authenticated user ID 7")
					}

					c.Status(http.StatusOK)
				},
			)

			request := httptest.NewRequest(
				http.MethodGet,
				"/private",
				nil,
			)
			if tt.header != "" {
				request.Header.Set("Authorization", tt.header)
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

			if tt.wantStatus == http.StatusOK {
				if !handlerCalled {
					t.Fatal("authenticated request should reach handler")
				}
				return
			}

			if handlerCalled {
				t.Fatal("unauthenticated request must not reach handler")
			}

			if recorder.Header().Get("WWW-Authenticate") != "Bearer" {
				t.Fatal("expected Bearer authentication challenge")
			}

			var response struct {
				Code string `json:"code"`
			}
			if err := json.Unmarshal(
				recorder.Body.Bytes(),
				&response,
			); err != nil {
				t.Fatal(err)
			}
			if response.Code != "unauthorized" {
				t.Fatalf("unexpected error code: %q", response.Code)
			}
		})
	}
}

func TestCurrentUserIDWithoutAuthentication(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)

	userID, ok := CurrentUserID(ctx)
	if ok || userID != 0 {
		t.Fatal("request without authentication must not have a user ID")
	}
}
