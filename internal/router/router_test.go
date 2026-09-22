package router

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"video-support-agent/internal/config"
)

func TestHealthEndpoint(t *testing.T) {
	cfg := config.Config{
		AppName:  "test-api",
		Env:      "test",
		Port:     "8080",
		LogLevel: "info",
	}

	r := New(cfg, nil)

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
