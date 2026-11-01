//go:build vectorhttpintegration

package router

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"video-support-agent/internal/cache"
	"video-support-agent/internal/config"
	"video-support-agent/internal/model"
	"video-support-agent/internal/retrieval"

	driver "github.com/go-sql-driver/mysql"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

type vectorRouteRetriever func(
	context.Context, string, retrieval.SearchOptions,
) ([]retrieval.HydratedChunk, error)

func (f vectorRouteRetriever) Retrieve(
	ctx context.Context, q string, opts retrieval.SearchOptions,
) ([]retrieval.HydratedChunk, error) {
	return f(ctx, q, opts)
}

type vectorRouteLimiter func(context.Context, uint64) (cache.RateLimitResult, error)

func (f vectorRouteLimiter) Allow(
	ctx context.Context, id uint64,
) (cache.RateLimitResult, error) {
	return f(ctx, id)
}

const vectorRoutePath = "/api/v1/knowledge/vector-search"
const vectorRouteSecret = "private-vector-query-and-backend-detail"

func vectorRouteMust(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		message := err.Error()
		if password := os.Getenv("ES_TEST_MYSQL_ROOT_PASSWORD"); password != "" {
			message = strings.ReplaceAll(message, password, "<redacted>")
		}
		t.Fatal(message)
	}
}

func vectorRouteDB(t *testing.T) *gorm.DB {
	t.Helper()
	if os.Getenv("RUN_VECTOR_HTTP_MYSQL") != "1" ||
		os.Getenv("INDEX_EXPORT_MYSQL_ADDR") != "127.0.0.1:13308" ||
		os.Getenv("INDEX_EXPORT_EXPECT_SERVER_UUID") !=
			"9ae2c7f4-d375-11f1-8945-ce027cf8cce1" ||
		os.Getenv("ES_TEST_MYSQL_ROOT_PASSWORD") == "" {
		t.Fatal("explicit isolated MySQL configuration is required")
	}

	open := func(name string) *sql.DB {
		cfg := driver.NewConfig()
		cfg.User = "root"
		cfg.Passwd = os.Getenv("ES_TEST_MYSQL_ROOT_PASSWORD")
		cfg.Net = "tcp"
		cfg.Addr = "127.0.0.1:13308"
		cfg.DBName = name
		cfg.ParseTime = true
		cfg.Loc = time.UTC
		cfg.Timeout = 5 * time.Second
		cfg.ReadTimeout = 5 * time.Second
		cfg.WriteTimeout = 5 * time.Second
		cfg.Logger = &driver.NopLogger{}
		connector, err := driver.NewConnector(cfg)
		vectorRouteMust(t, err)
		pool := sql.OpenDB(connector)
		pool.SetMaxOpenConns(2)
		t.Cleanup(func() { _ = pool.Close() })
		return pool
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	admin := open("")
	var uuid, version string
	vectorRouteMust(t, admin.QueryRowContext(ctx,
		"SELECT @@server_uuid, VERSION()").Scan(&uuid, &version))
	if uuid != os.Getenv("INDEX_EXPORT_EXPECT_SERVER_UUID") ||
		!strings.HasPrefix(version, "8.4.") {
		t.Fatal("server identity differs; no database created")
	}

	var token [8]byte
	_, err := rand.Read(token[:])
	vectorRouteMust(t, err)
	name := "vec_http_it_" + hex.EncodeToString(token[:])
	_, err = admin.ExecContext(ctx,
		"CREATE DATABASE `"+name+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci")
	vectorRouteMust(t, err)
	t.Logf("TEST_DATABASE=%s SERVER_UUID=%s", name, uuid)
	t.Cleanup(func() {
		t.Logf("RESOURCES_RETAINED=true TEST_DATABASE=%s", name)
	})

	db, err := gorm.Open(gormmysql.New(gormmysql.Config{
		Conn: open(name), SkipInitializeWithVersion: true,
	}), &gorm.Config{
		DisableAutomaticPing: true,
		Logger:               gormlogger.Default.LogMode(gormlogger.Silent),
	})
	vectorRouteMust(t, err)
	vectorRouteMust(t, db.WithContext(ctx).
		Set("gorm:table_options", "ENGINE=InnoDB").AutoMigrate(&model.User{}))
	return db
}

func vectorRouteRequest(r http.Handler, path, header string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path+"?q="+vectorRouteSecret, nil)
	if header != "" {
		req.Header.Set("Authorization", header)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestVectorRouteRegistration(t *testing.T) {
	cfg := config.Config{AppName: "vector-route-test", Env: "test"}
	tokens := searchRouteTokens(t, strings.Repeat("a", 32))
	calls := 0
	vector := vectorRouteRetriever(func(
		context.Context, string, retrieval.SearchOptions,
	) ([]retrieval.HydratedChunk, error) {
		calls++
		return []retrieval.HydratedChunk{}, nil
	})
	old := New(cfg, &gorm.DB{}, tokens, nil, nil, nil, nil, nil)
	compat := NewWithRetrievers(cfg, &gorm.DB{}, tokens,
		nil, nil, nil, nil, nil, nil, nil)
	if !reflect.DeepEqual(searchRouteList(old), searchRouteList(compat)) {
		t.Fatal("nil vector dependency changed compatibility routes")
	}
	withVector := NewWithRetrievers(cfg, &gorm.DB{}, tokens,
		nil, nil, nil, nil, nil, nil, vector)
	want := append(searchRouteList(old), "GET "+vectorRoutePath)
	sort.Strings(want)
	if !reflect.DeepEqual(searchRouteList(withVector), want) || calls != 0 {
		t.Fatal("unexpected registrations or constructor retrieval call")
	}

	healthOnly := NewWithRetrievers(cfg, nil, tokens,
		nil, nil, nil, nil, nil, nil, vector)
	if w := vectorRouteRequest(healthOnly, vectorRoutePath, ""); w.Code != 404 {
		t.Fatal("database-free router exposed vector route")
	}
	w := httptest.NewRecorder()
	healthOnly.ServeHTTP(w, httptest.NewRequest("GET", "/health", nil))
	if w.Code != 200 || calls != 0 {
		t.Fatal("health-only compatibility differs")
	}
}

func TestVectorRouteMySQL(t *testing.T) {
	db := vectorRouteDB(t)
	cfg := config.Config{AppName: "vector-route-test", Env: "test"}
	tokens := searchRouteTokens(t, strings.Repeat("a", 32))
	wrongSigner := searchRouteTokens(t, strings.Repeat("b", 32))

	for _, mode := range []string{
		"user", "support", "admin", "blocked_role", "missing_user",
		"missing_token", "malformed_token", "wrong_signature",
		"limited", "limiter_error", "backend_error", "panic", "es",
	} {
		t.Run(mode, func(t *testing.T) {
			role := model.RoleUser
			if mode == "support" || mode == "admin" {
				role = mode
			}
			if mode == "blocked_role" {
				role = "blocked"
			}
			user := model.User{
				Username:     "vector_" + mode,
				PasswordHash: "isolated-test-placeholder", Role: role,
			}
			vectorRouteMust(t, db.Create(&user).Error)
			id := user.ID
			if mode == "missing_user" {
				id = ^uint64(0)
			}
			token, err := tokens.Issue(id)
			vectorRouteMust(t, err)
			header := "Bearer " + token
			switch mode {
			case "missing_token":
				header = ""
			case "malformed_token":
				header = "Bearer private-invalid-token"
			case "wrong_signature":
				token, err = wrongSigner.Issue(id)
				vectorRouteMust(t, err)
				header = "Bearer " + token
			}

			core, logs := observer.New(zap.InfoLevel)
			limitCalls, vectorCalls, esCalls := 0, 0, 0
			limiter := vectorRouteLimiter(func(
				ctx context.Context, gotID uint64,
			) (cache.RateLimitResult, error) {
				limitCalls++
				deadline, ok := ctx.Deadline()
				if gotID != id || !ok || time.Until(deadline) > 500*time.Millisecond {
					t.Fatal("limiter identity/deadline differs")
				}
				if mode == "limiter_error" {
					return cache.RateLimitResult{}, errors.New("limiter unavailable")
				}
				return cache.RateLimitResult{
					Allowed: mode != "limited", RetryAfter: 1500 * time.Millisecond,
				}, nil
			})
			vector := vectorRouteRetriever(func(
				ctx context.Context, q string, opts retrieval.SearchOptions,
			) ([]retrieval.HydratedChunk, error) {
				vectorCalls++
				deadline, ok := ctx.Deadline()
				if q != vectorRouteSecret || opts != (retrieval.SearchOptions{Limit: 10}) ||
					!ok || time.Until(deadline) <= 0 ||
					time.Until(deadline) > 40*time.Second {
					t.Fatal("retrieval parameters/deadline differ")
				}
				if mode == "panic" {
					panic(vectorRouteSecret)
				}
				if mode == "backend_error" {
					return nil, fmt.Errorf("%s: %w",
						vectorRouteSecret, retrieval.ErrHydrationFailed)
				}
				return []retrieval.HydratedChunk{}, nil
			})
			es := searchRouteRetriever(func(
				context.Context, string, retrieval.SearchOptions,
			) ([]retrieval.RuleChunk, error) {
				esCalls++
				return []retrieval.RuleChunk{}, nil
			})
			r := NewWithRetrievers(cfg, db, tokens, nil,
				limiter, nil, zap.New(core), nil, es, vector)
			path := vectorRoutePath
			if mode == "es" {
				path = "/api/v1/knowledge/search"
			}
			w := vectorRouteRequest(r, path, header)

			status, code := 200, ""
			wantVector, wantES, wantLimit := 1, 0, 1
			switch mode {
			case "blocked_role":
				status, code, wantVector = 403, "forbidden", 0
			case "missing_user":
				status, code, wantVector = 401, "unauthorized", 0
			case "missing_token", "malformed_token", "wrong_signature":
				status, code, wantVector, wantLimit = 401, "unauthorized", 0, 0
			case "limited":
				status, code, wantVector = 429, "rate_limited", 0
			case "limiter_error":
				status, code, wantVector = 503, "rate_limit_unavailable", 0
			case "backend_error":
				status, code = 503, "search_hydration_failed"
			case "panic":
				status, code = 500, "internal_error"
			case "es":
				wantVector, wantES = 0, 1
			}
			if w.Code != status || vectorCalls != wantVector ||
				esCalls != wantES || limitCalls != wantLimit {
				t.Fatalf("status=%d calls(vector/es/limit)=%d/%d/%d body=%s",
					w.Code, vectorCalls, esCalls, limitCalls, w.Body.String())
			}
			var body map[string]any
			vectorRouteMust(t, json.Unmarshal(w.Body.Bytes(), &body))
			if status == 200 {
				want := map[string]any{
					"items": []any{}, "count": float64(0), "limit": float64(10),
				}
				if !reflect.DeepEqual(body, want) {
					t.Fatalf("unexpected success response: %v", body)
				}
			} else if len(body) != 2 || body["code"] != code || body["message"] == "" {
				t.Fatalf("unexpected error response: %v", body)
			}
			if status == 401 && w.Header().Get("WWW-Authenticate") != "Bearer" {
				t.Fatal("missing authentication challenge")
			}
			if status == 429 && w.Header().Get("Retry-After") != "2" {
				t.Fatal("rate-limit retry delay differs")
			}

			accessCount := 0
			for _, entry := range logs.All() {
				fields := entry.ContextMap()
				if entry.Message == "http request" {
					accessCount++
					if fields["route"] != path || fields["status"] != int64(status) {
						t.Fatal("access log route/status differs")
					}
				}
				encoded, err := json.Marshal(fields)
				vectorRouteMust(t, err)
				for _, secret := range []string{
					vectorRouteSecret, token, "private-invalid-token",
				} {
					if strings.Contains(entry.Message+string(encoded), secret) ||
						strings.Contains(w.Body.String(), secret) {
						t.Fatal("query, token, panic or backend detail leaked")
					}
				}
			}
			if accessCount != 1 {
				t.Fatal("expected exactly one safe access log")
			}
		})
	}

	t.Run("latest_database_role_with_same_token", func(t *testing.T) {
		user := model.User{
			Username:     "vector_role_changes",
			PasswordHash: "isolated-test-placeholder", Role: model.RoleUser,
		}
		vectorRouteMust(t, db.Create(&user).Error)
		token, err := tokens.Issue(user.ID)
		vectorRouteMust(t, err)
		calls := 0
		vector := vectorRouteRetriever(func(
			context.Context, string, retrieval.SearchOptions,
		) ([]retrieval.HydratedChunk, error) {
			calls++
			return []retrieval.HydratedChunk{}, nil
		})
		limiter := vectorRouteLimiter(func(
			context.Context, uint64,
		) (cache.RateLimitResult, error) {
			return cache.RateLimitResult{Allowed: true}, nil
		})
		r := NewWithRetrievers(cfg, db, tokens, nil, limiter,
			nil, nil, nil, nil, vector)
		for _, step := range []struct {
			role   string
			status int
			calls  int
		}{
			{model.RoleUser, 200, 1},
			{"blocked", 403, 1},
			{model.RoleAdmin, 200, 2},
		} {
			vectorRouteMust(t, db.Model(&model.User{}).
				Where("id = ?", user.ID).Update("role", step.role).Error)
			w := vectorRouteRequest(r, vectorRoutePath, "Bearer "+token)
			if w.Code != step.status || calls != step.calls {
				t.Fatal("request did not use latest database role")
			}
		}
	})
}
