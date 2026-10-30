package router

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"video-support-agent/internal/auth"
	"video-support-agent/internal/config"
	"video-support-agent/internal/retrieval"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"gorm.io/gorm"
)

type searchRouteRetriever func(
	context.Context, string, retrieval.SearchOptions,
) ([]retrieval.RuleChunk, error)

func (f searchRouteRetriever) Retrieve(
	ctx context.Context, query string, opts retrieval.SearchOptions,
) ([]retrieval.RuleChunk, error) {
	return f(ctx, query, opts)
}

func searchRouteTokens(t *testing.T, secret string) *auth.TokenManager {
	t.Helper()
	manager, err := auth.NewTokenManager(secret, "search-route-test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

func searchRouteList(r *gin.Engine) []string {
	var routes []string
	for _, route := range r.Routes() {
		routes = append(routes, route.Method+" "+route.Path)
	}
	sort.Strings(routes)
	return routes
}

func TestSearchRouteRegistrationAndCompatibility(t *testing.T) {
	cfg := config.Config{AppName: "search-test", Env: "test"}
	tokens := searchRouteTokens(t, strings.Repeat("a", 32))
	calls := 0
	retriever := searchRouteRetriever(func(
		context.Context, string, retrieval.SearchOptions,
	) ([]retrieval.RuleChunk, error) {
		calls++
		return []retrieval.RuleChunk{}, nil
	})

	// 仅用于注册路由；本测试不发送已认证的数据库请求。
	oldRouter := New(cfg, &gorm.DB{}, tokens, nil, nil, nil, nil, nil)
	newRouter := NewWithRetriever(
		cfg, &gorm.DB{}, tokens, nil, nil, nil, nil, nil, retriever,
	)
	oldRoutes := searchRouteList(oldRouter)
	newRoutes := searchRouteList(newRouter)
	if !reflect.DeepEqual(oldRoutes, newRoutes) {
		t.Fatalf("compatibility wrapper changed routes:\nold=%v\nnew=%v",
			oldRoutes, newRoutes)
	}
	count := 0
	for _, route := range newRouter.Routes() {
		if route.Path == "/api/v1/knowledge/search" {
			count++
			if route.Method != http.MethodGet {
				t.Fatalf("unexpected search method: %s", route.Method)
			}
		}
	}
	if count != 1 || calls != 0 {
		t.Fatalf("search registrations=%d, constructor retrieval calls=%d", count, calls)
	}
}

func TestSearchRouteAuthenticationAndLogPrivacy(t *testing.T) {
	cfg := config.Config{AppName: "search-test", Env: "test"}
	tokens := searchRouteTokens(t, strings.Repeat("a", 32))
	wrongSigner := searchRouteTokens(t, strings.Repeat("b", 32))
	badSignature, err := wrongSigner.Issue(99)
	if err != nil {
		t.Fatal(err)
	}

	const querySecret = "private-search-query"
	const tokenSecret = "private-invalid-token"

	for _, constructor := range []string{"compatibility", "injected"} {
		for _, tc := range []struct {
			name   string
			header string
		}{
			{"missing", ""},
			{"wrong_scheme", "Basic " + tokenSecret},
			{"malformed", "Bearer " + tokenSecret},
			{"wrong_signature", "Bearer " + badSignature},
		} {
			t.Run(constructor+"/"+tc.name, func(t *testing.T) {
				core, logs := observer.New(zap.InfoLevel)
				logger := zap.New(core)
				calls := 0
				retriever := searchRouteRetriever(func(
					context.Context, string, retrieval.SearchOptions,
				) ([]retrieval.RuleChunk, error) {
					calls++
					return []retrieval.RuleChunk{}, nil
				})

				var r *gin.Engine
				if constructor == "compatibility" {
					r = New(cfg, &gorm.DB{}, tokens, nil, nil, nil, logger, nil)
				} else {
					r = NewWithRetriever(
						cfg, &gorm.DB{}, tokens, nil, nil, nil, logger, nil, retriever,
					)
				}

				req := httptest.NewRequest(http.MethodGet,
					"/api/v1/knowledge/search?q="+querySecret, nil)
				if tc.header != "" {
					req.Header.Set("Authorization", tc.header)
				}
				w := httptest.NewRecorder()
				r.ServeHTTP(w, req)

				if w.Code != 401 || w.Header().Get("WWW-Authenticate") != "Bearer" {
					t.Fatalf("expected JWT rejection: status=%d body=%s",
						w.Code, w.Body.String())
				}
				var body map[string]any
				if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
					t.Fatal(err)
				}
				want := map[string]any{
					"code":    "unauthorized",
					"message": "authentication required or token invalid",
				}
				if !reflect.DeepEqual(body, want) || calls != 0 {
					t.Fatalf("authentication did not stop request: body=%v calls=%d", body, calls)
				}

				entries := logs.All()
				if len(entries) != 1 || entries[0].Message != "http request" {
					t.Fatalf("unexpected log entries: %d", len(entries))
				}
				fields := entries[0].ContextMap()
				if fields["route"] != "/api/v1/knowledge/search" ||
					fields["method"] != http.MethodGet ||
					fields["status"] != int64(401) {
					t.Fatalf("unexpected access log fields: %v", fields)
				}
				encoded, err := json.Marshal(fields)
				if err != nil {
					t.Fatal(err)
				}
				for _, secret := range []string{querySecret, tokenSecret, badSignature} {
					if strings.Contains(string(encoded), secret) ||
						strings.Contains(entries[0].Message, secret) ||
						strings.Contains(w.Body.String(), secret) {
						t.Fatal("query or authorization value leaked")
					}
				}
			})
		}
	}
}

func TestSearchRouteWithoutDatabase(t *testing.T) {
	cfg := config.Config{AppName: "search-test", Env: "test"}
	calls := 0
	r := NewWithRetriever(cfg, nil, nil, nil, nil, nil, nil, nil,
		searchRouteRetriever(func(
			context.Context, string, retrieval.SearchOptions,
		) ([]retrieval.RuleChunk, error) {
			calls++
			return []retrieval.RuleChunk{}, nil
		}),
	)

	for _, tc := range []struct {
		path   string
		status int
	}{
		{"/health", 200},
		{"/api/v1/knowledge/search?q=test", 404},
	} {
		t.Run(tc.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if w.Code != tc.status {
				t.Fatalf("status=%d, want=%d", w.Code, tc.status)
			}
		})
	}
	if calls != 0 {
		t.Fatal("health-only router invoked retriever")
	}
}
