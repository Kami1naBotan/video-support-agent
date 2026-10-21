package router

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"video-support-agent/internal/config"

	"gorm.io/gorm"
)

func TestHealthEndpoint(t *testing.T) {
	cfg := config.Config{
		AppName:  "test-api",
		Env:      "test",
		Port:     "8080",
		LogLevel: "info",
	}

	r := New(cfg, nil, nil, nil, nil, nil, nil, nil)

	request := httptest.NewRequest(http.MethodGet, "/health", nil)
	recorder := httptest.NewRecorder()

	r.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, recorder.Code)
	}

	expectedBody := `{"service":"test-api","status":"ok"}`
	if recorder.Body.String() != expectedBody {
		t.Fatalf("expected body %s, got %s", expectedBody, recorder.Body.String())
	}
}

func TestStreamChatRouteRegistered(t *testing.T) {
	cfg := config.Config{
		AppName: "test-api",
		Env:     "test",
	}

	// 这里只检查路由注册，不执行请求或访问数据库。
	// 非 nil 的占位对象让 New 注册业务路由。
	r := New(
		cfg,
		&gorm.DB{},
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
	)

	const wantPath = "/api/v1/conversations/:id/messages/stream"
	const wrongPath = "/api/v1/conversations/:id/message/stream"

	found := false

	for _, route := range r.Routes() {
		if route.Method != http.MethodPost {
			continue
		}

		if route.Path == wrongPath {
			t.Errorf("unexpected singular route: %s", route.Path)
		}

		if route.Path == wantPath {
			found = true
		}
	}

	if !found {
		t.Errorf("missing POST route: %s", wantPath)
	}
}
