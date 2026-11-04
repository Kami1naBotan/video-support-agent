package router

import (
	"testing"

	"video-support-agent/internal/config"
	"video-support-agent/internal/handler"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func TestAgentRoutesRegistered(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// 这里只构造路由，不执行仓储请求，不连接数据库。
	r := NewWithMessageHandler(
		config.Config{AppName: "test-api", Env: "test"},
		&gorm.DB{},
		nil, nil, nil, nil, nil, nil, nil, nil,
		&handler.MessageHandler{},
	)

	want := map[string]bool{
		"POST /api/v1/conversations/:id/messages":          false,
		"POST /api/v1/conversations/:id/messages/stream":   false,
		"GET /api/v1/conversations/:id/messages":           false,
		"GET /api/v1/conversations/:id/agent-runs/:run_id": false,
	}
	for _, route := range r.Routes() {
		key := route.Method + " " + route.Path
		if _, required := want[key]; required {
			want[key] = true
		}
	}
	for route, found := range want {
		if !found {
			t.Errorf("required route not registered: %s", route)
		}
	}
}
