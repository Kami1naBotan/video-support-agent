//go:build loadtest

package loadacceptance

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"video-support-agent/internal/auth"
	"video-support-agent/internal/cache"
	"video-support-agent/internal/config"
	"video-support-agent/internal/database"
	"video-support-agent/internal/model"
	"video-support-agent/internal/router"
)

// Measures the production router in a native test process, without race instrumentation.
// Publication is seeded before timing; login, API executable startup and LLM are excluded.
func TestKnowledgeReadLoad(t *testing.T) {
	require.Equal(t, "1", os.Getenv("TEST_ISOLATED"))
	evidence := os.Getenv("LOAD_EVIDENCE")
	require.NotEmpty(t, evidence)
	root, err := filepath.Abs("../..")
	require.NoError(t, err)
	random := make([]byte, 24)
	_, err = rand.Read(random)
	require.NoError(t, err)
	password := hex.EncodeToString(random)
	owner := "knowledge-load-" + password[:12]
	save := func(name string, value any) {
		b, e := json.MarshalIndent(value, "", "  ")
		require.NoError(t, e)
		require.NoError(t, os.WriteFile(filepath.Join(evidence, name), b, 0600))
	}
	run := func(env []string, args ...string) string {
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
		defer cancel()
		c := exec.CommandContext(ctx, "docker", args...)
		c.Env = append(os.Environ(), env...)
		b, e := c.CombinedOutput()
		if e != nil {
			t.Fatalf("docker %s failed: %v: %s", args[0], e, strings.ReplaceAll(string(b), password, "[REDACTED]"))
		}
		return strings.TrimSpace(string(b))
	}
	create := func(name, image, port string, extra ...string) string {
		args := []string{"run", "-d", "--name", name, "--label", "load.owner=" + owner, "-p", "127.0.0.1::" + port}
		args = append(args, extra...)
		args = append(args, image)
		run([]string{"MYSQL_ROOT_PASSWORD=" + password, "MYSQL_PASSWORD=" + password}, args...)
		t.Cleanup(func() {
			require.Equal(t, owner, run(nil, "inspect", "--format", `{{index .Config.Labels "load.owner"}}`, name))
			run(nil, "rm", "-f", "-v", name)
			require.Empty(t, run(nil, "ps", "-aq", "--filter", "name=^/"+name+"$"))
		})
		addr := run(nil, "port", name, port+"/tcp")
		require.Regexp(t, `^127\.0\.0\.1:\d+$`, addr)
		return addr
	}
	mysqlName := owner + "-mysql"
	addr := create(mysqlName, "mysql:8.4.11", "3306", "-e", "MYSQL_ROOT_PASSWORD", "-e", "MYSQL_PASSWORD", "-e", "MYSQL_DATABASE=bili_support_test", "-e", "MYSQL_USER=regression")
	redisAddr := create(owner+"-redis", "redis:7.4.7-alpine", "6379")
	require.Equal(t, owner, run(nil, "inspect", "--format", `{{index .Config.Labels "load.owner"}}`, mysqlName))
	require.Equal(t, owner, run(nil, "inspect", "--format", `{{index .Config.Labels "load.owner"}}`, owner+"-redis"))
	// Readiness queries execute only inside the newly created owned container.
	ready := false
	for i := 0; i < 60; i++ {
		c := exec.Command("docker", "exec", mysqlName, "sh", "-c", `MYSQL_PWD="$MYSQL_PASSWORD" mysql -uregression -Dbili_support_test -N -B -e 'SELECT @@server_uuid'`)
		b, e := c.CombinedOutput()
		if e == nil && len(strings.TrimSpace(string(b))) == 36 {
			ready = true
			break
		}
		time.Sleep(time.Second)
	}
	require.True(t, ready, "isolated MySQL readiness")
	uuid := run(nil, "exec", mysqlName, "sh", "-c", `MYSQL_PWD="$MYSQL_PASSWORD" mysql -uregression -Dbili_support_test -N -B -e 'SELECT @@server_uuid'`)
	db, err := database.OpenMySQL(config.DatabaseConfig{Host: "127.0.0.1", Port: strings.Split(addr, ":")[1], User: "regression", Password: password, Name: "bili_support_test"})
	require.NoError(t, err)
	pool, err := db.DB()
	require.NoError(t, err)
	defer pool.Close()
	var identity struct{ Name, UUID string }
	require.NoError(t, db.Raw("SELECT DATABASE() AS name, @@server_uuid AS uuid").Scan(&identity).Error)
	require.Equal(t, "bili_support_test", identity.Name)
	require.Equal(t, uuid, identity.UUID)
	require.NoError(t, database.Migrate(db))
	rc := redis.NewClient(&redis.Options{Addr: redisAddr})
	defer rc.Close()
	require.NoError(t, rc.Ping(context.Background()).Err())
	limiter, err := cache.NewUserRateLimiter(rc, 60, time.Minute)
	require.NoError(t, err)
	authLimiter, err := cache.NewUserRateLimiter(rc, 10, time.Minute)
	require.NoError(t, err)
	tokens, err := auth.NewTokenManager(password, "isolated-load", 10*time.Minute)
	require.NoError(t, err)
	var credentials []string
	for i := 0; i < 11; i++ {
		u := model.User{Username: fmt.Sprintf("load-user-%d", i), PasswordHash: "unused-test-hash", Role: model.RoleUser}
		require.NoError(t, db.Create(&u).Error)
		token, e := tokens.Issue(u.ID)
		require.NoError(t, e)
		credentials = append(credentials, token)
	}
	category := model.RuleCategory{Code: owner, Name: "Synthetic load"}
	require.NoError(t, db.Create(&category).Error)
	doc := model.KnowledgeDocument{DocumentKey: owner, PublicationRevision: 1}
	require.NoError(t, db.Create(&doc).Error)
	now := time.Now().UTC().Add(-time.Minute)
	content := "Synthetic isolated knowledge load article."
	version := model.RuleVersion{DocumentID: doc.ID, CategoryID: category.ID, Version: "v1", ContentRevision: 1, Status: model.RuleVersionStatusPublished, PublishedAt: &now, EffectiveAt: &now, Title: "Synthetic load", Content: content, SourceURL: "https://www.bilibili.com/blackboard/test"}
	require.NoError(t, db.Omit("Document", "Category").Create(&version).Error)
	article := model.RuleArticle{VersionID: version.ID, ArticleOrder: 1, SectionPath: "test", ArticleNumber: "1", Content: content}
	require.NoError(t, db.Omit("Version").Create(&article).Error)
	require.NoError(t, db.Model(&doc).Update("current_version_id", version.ID).Error)
	gin.SetMode(gin.ReleaseMode)
	server := httptest.NewServer(router.New(config.Config{AppName: "isolated-load"}, db, tokens, nil, limiter, authLimiter, zap.NewNop(), nil))
	defer server.Close()
	path := fmt.Sprintf("/api/v1/knowledge/documents/%d/current", doc.ID)
	client := &http.Client{Timeout: 5 * time.Second}
	probe := func(token string, status int) {
		req, e := http.NewRequest("GET", server.URL+path, nil)
		require.NoError(t, e)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res, e := client.Do(req)
		require.NoError(t, e)
		defer res.Body.Close()
		require.Equal(t, status, res.StatusCode)
		b, e := io.ReadAll(res.Body)
		require.NoError(t, e)
		if status == 200 {
			var body struct {
				DocumentID uint64 `json:"document_id"`
				VersionID  uint64 `json:"version_id"`
				Articles   []struct {
					ID      uint64 `json:"article_id"`
					Content string `json:"content"`
				} `json:"articles"`
			}
			require.NoError(t, json.Unmarshal(b, &body))
			require.Equal(t, doc.ID, body.DocumentID)
			require.Equal(t, version.ID, body.VersionID)
			require.Len(t, body.Articles, 1)
			require.Equal(t, article.ID, body.Articles[0].ID)
			require.Equal(t, content, body.Articles[0].Content)
		}
	}
	probe("", 401)
	warmStart := time.Now()
	for i := 0; i < 10; i++ {
		probe(credentials[10], 200)
	}
	save("warmup.json", map[string]any{"requests": 10, "success": 10, "elapsed_ms": time.Since(warmStart).Milliseconds(), "separate_user": true, "excluded_from_k6": true, "unauthenticated_status": 401})
	save("environment.json", map[string]any{"go": runtime.Version(), "os": runtime.GOOS, "arch": runtime.GOARCH, "logical_cpus": runtime.NumCPU(), "gomaxprocs": runtime.GOMAXPROCS(0), "race": false, "server_uuid": uuid, "owner": owner, "mysql": addr, "redis": redisAddr, "base_url": server.URL, "users": 10, "rate_per_second": 5, "duration_seconds": 60, "pool_max_open": 10, "pool_max_idle": 5, "user_limit_per_minute": 60, "scope": "production router/auth/roles/Redis limiter/MySQL read in httptest native process; seeded publication; no login/cmd-api startup/LLM; observation disabled; nop access logger", "images": run(nil, "image", "inspect", "--format", "{{.Id}} {{json .RepoDigests}}", "mysql:8.4.11", "redis:7.4.7-alpine", "grafana/k6:0.57.0")})
	tokenJSON, err := json.Marshal(credentials[:10])
	require.NoError(t, err)
	env := []string{"TEST_ISOLATED=1", "LOAD_BASE_URL=" + server.URL, fmt.Sprintf("LOAD_DOCUMENT_ID=%d", doc.ID), fmt.Sprintf("LOAD_VERSION_ID=%d", version.ID), fmt.Sprintf("LOAD_ARTICLE_ID=%d", article.ID), "LOAD_EXPECTED_CONTENT=" + content, "LOAD_TOKENS=" + string(tokenJSON)}
	args := []string{"run", "--rm", "--name", owner + "-k6", "--label", "load.owner=" + owner, "--network", "host", "-v", filepath.Join(root, ".scratch/test/script") + ":/scripts:ro", "-v", evidence + ":/results", "-w", "/results"}
	for _, v := range env {
		k, _, _ := strings.Cut(v, "=")
		args = append(args, "-e", k)
	}
	args = append(args, "grafana/k6:0.57.0", "run", "--out", "json=/results/metrics.json", "/scripts/knowledge-read-load.js")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Env = append(os.Environ(), env...)
	output, e := cmd.CombinedOutput()
	require.NoError(t, os.WriteFile(filepath.Join(evidence, "k6.log"), output, 0600))
	// A timed-out client must not leave its load generator alive.
	remaining := run(nil, "ps", "-aq", "--filter", "label=load.owner="+owner, "--filter", "name="+owner+"-k6")
	if remaining != "" {
		run(nil, "rm", "-f", owner+"-k6")
	}
	require.NoError(t, e, "k6 failed; see retained k6.log and summary.json")
	raw, err := os.ReadFile(filepath.Join(evidence, "summary.json"))
	require.NoError(t, err)
	var summary struct {
		Metrics map[string]struct {
			Values     map[string]float64 `json:"values"`
			Thresholds map[string]struct {
				OK bool `json:"ok"`
			} `json:"thresholds"`
		} `json:"metrics"`
	}
	require.NoError(t, json.Unmarshal(raw, &summary))
	for _, name := range []string{"checks", "business_failure_rate", "http_req_failed", "dropped_iterations"} {
		metric, ok := summary.Metrics[name]
		require.True(t, ok, name)
		require.NotEmpty(t, metric.Thresholds)
		for _, th := range metric.Thresholds {
			require.True(t, th.OK, name)
		}
	}
	require.GreaterOrEqual(t, summary.Metrics["http_reqs"].Values["count"], float64(299))
	save("db-pool-after.json", pool.Stats())
	t.Log("k6 correctness thresholds passed; latency is descriptive, not an SLO")
}
