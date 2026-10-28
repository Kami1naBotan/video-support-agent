package router

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"video-support-agent/internal/config"

	"gorm.io/gorm"
)

func TestCreateKnowledgeDocumentRequiresLogin(t *testing.T) {
	cfg := config.Config{AppName: "test-api", Env: "test"}

	// 非 nil 占位对象用于注册业务路由；本用例必须在认证阶段结束，不能访问数据库。
	r := New(cfg, &gorm.DB{}, nil, nil, nil, nil, nil, nil)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/admin/knowledge/documents", nil)
	recorder := httptest.NewRecorder()
	r.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("expected status %d, got %d; body=%s", http.StatusUnauthorized, recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("WWW-Authenticate"); got != "Bearer" {
		t.Errorf("expected Bearer authentication challenge, got %q", got)
	}
	var response struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode authentication response: %v", err)
	}
	if response.Code != "unauthorized" {
		t.Errorf("expected unauthorized error code, got %q", response.Code)
	}
}
