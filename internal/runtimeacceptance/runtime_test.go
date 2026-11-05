//go:build runtimee2e

package runtimeacceptance

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	driver "github.com/go-sql-driver/mysql"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"video-support-agent/internal/auth"
	"video-support-agent/internal/config"
	"video-support-agent/internal/embedding"
	"video-support-agent/internal/model"
	"video-support-agent/internal/repository"
)

const digest = "7907646426070047a77226ac3e684fbbe8410524f7b4a74d02837e43f2146bab"

func must(t *testing.T, e error) {
	t.Helper()
	if e != nil {
		t.Fatal(e)
	}
}
func until(t *testing.T, name string, seconds int, f func() bool) {
	t.Helper()
	end := time.Now().Add(time.Duration(seconds) * time.Second)
	for time.Now().Before(end) {
		if f() {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatal("timeout: " + name)
}
func save(t *testing.T, name string, v any) {
	t.Helper()
	b, e := json.MarshalIndent(v, "", "  ")
	must(t, e)
	must(t, os.WriteFile(filepath.Join(os.Getenv("RUNTIME_EVIDENCE"), name+".json"), b, 0600))
}

type running struct {
	cmd     *exec.Cmd
	done    chan error
	stopped bool
}

func start(t *testing.T, kind, name string, env map[string]string) *running {
	t.Helper()
	cmd := exec.Command(os.Getenv("RUNTIME_" + kind + "_BINARY"))
	cmd.Dir = os.Getenv("RUNTIME_ROOT")
	values := map[string]string{}
	for _, s := range os.Environ() {
		k, v, _ := strings.Cut(s, "=")
		values[k] = v
	}
	for k, v := range env {
		values[k] = v
	}
	for k, v := range values {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	f, e := os.Create(filepath.Join(os.Getenv("RUNTIME_EVIDENCE"), name+".log"))
	must(t, e)
	cmd.Stdout = f
	cmd.Stderr = f
	must(t, cmd.Start())
	p := &running{cmd: cmd, done: make(chan error, 1)}
	go func() { p.done <- cmd.Wait(); f.Close() }()
	t.Cleanup(func() { p.stop(t) })
	t.Logf("started %s pid=%d", name, cmd.Process.Pid)
	return p
}
func (p *running) stop(t *testing.T) {
	t.Helper()
	if p.stopped {
		return
	}
	p.stopped = true
	_ = p.cmd.Process.Kill()
	select {
	case <-p.done:
	case <-time.After(15 * time.Second):
		t.Error("owned process did not exit")
	}
}
func TestRuntimeEventFlow(t *testing.T) {
	if os.Getenv("TEST_ISOLATED") != "1" || os.Getenv("DB_HOST") != "127.0.0.1" || os.Getenv("DB_NAME") != "bili_support_test" || os.Getenv("RUNTIME_EVIDENCE") == "" {
		t.Fatal("isolated runtime script required")
	}
	d := driver.NewConfig()
	d.Net = "tcp"
	d.Addr = net.JoinHostPort(os.Getenv("DB_HOST"), os.Getenv("DB_PORT"))
	d.User = os.Getenv("DB_USER")
	d.Passwd = os.Getenv("DB_PASSWORD")
	d.DBName = os.Getenv("DB_NAME")
	d.ParseTime = true
	d.Timeout = 5 * time.Second
	d.ReadTimeout = 10 * time.Second
	d.WriteTimeout = 10 * time.Second
	db, e := gorm.Open(mysql.Open(d.FormatDSN()), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	must(t, e)
	pool, e := db.DB()
	must(t, e)
	defer pool.Close()
	var identity struct{ Name, UUID string }
	must(t, db.Raw("SELECT DATABASE() AS name, @@server_uuid AS uuid").Scan(&identity).Error)
	if identity.Name != d.DBName || identity.UUID != os.Getenv("TEST_MYSQL_SERVER_UUID") {
		t.Fatal("source identity mismatch")
	}
	var count int64
	must(t, db.Model(&model.KnowledgeDocument{}).Count(&count).Error)
	if count != 0 {
		t.Fatal("fresh database required")
	}
	category := model.RuleCategory{Code: "runtime-synthetic", Name: "虚构验收"}
	must(t, db.Create(&category).Error)
	admin := model.User{Username: "runtime-admin", PasswordHash: "disabled", Role: model.RoleAdmin}
	user := model.User{Username: "runtime-user", PasswordHash: "disabled", Role: model.RoleUser}
	must(t, db.Create(&admin).Error)
	must(t, db.Create(&user).Error)
	secret := os.Getenv("DB_PASSWORD")
	tokens, e := auth.NewTokenManager(secret, "runtime-test", time.Hour)
	must(t, e)
	adminToken, e := tokens.Issue(admin.ID)
	must(t, e)
	userToken, e := tokens.Issue(user.ID)
	must(t, e)
	var unexpectedLLM atomic.Int32
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		unexpectedLLM.Add(1)
		http.Error(w, "unexpected conversation request", 500)
	}))
	defer llm.Close()
	l, e := net.Listen("tcp", "127.0.0.1:0")
	must(t, e)
	port := strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
	l.Close()
	base := "http://127.0.0.1:" + port
	env := map[string]string{"APP_PORT": port, "APP_ENV": "production", "JWT_SECRET": secret, "JWT_ISSUER": "runtime-test", "APPEAL_CONFIRMATION_KEY_BASE64": base64.StdEncoding.EncodeToString([]byte(secret)[:32]), "LLM_BASE_URL": llm.URL + "/v1", "LLM_MODEL": "test-unused", "LLM_API_KEY": "test-unused", "LLM_TIMEOUT": "10s", "REDIS_ADDR": os.Getenv("RUNTIME_REDIS_ADDR"), "REDIS_PASSWORD": "", "KNOWLEDGE_SOURCE_SERVER_UUID": identity.UUID, "KNOWLEDGE_GENERATION_LIMIT": "100", "KNOWLEDGE_ES_DEFINITION": "deploy/elasticsearch/rule-chunks-v1.json", "ES_URL": os.Getenv("DUAL_TEST_ES_URL"), "MILVUS_URL": os.Getenv("DUAL_TEST_MILVUS_URL"), "EMBEDDING_URL": "http://127.0.0.1:11434", "EMBEDDING_MODEL_DIGEST": digest, "KAFKA_BROKERS": os.Getenv("RUNTIME_KAFKA_ADDRESS"), "KNOWLEDGE_SOURCE_GROUP": "runtime-source", "KNOWLEDGE_RETRY_GROUP": "runtime-retry", "ES_TIMEOUT": "10s", "MILVUS_TIMEOUT": "10s", "LOG_LEVEL": "info"}
	embed, e := embedding.NewClient(config.EmbeddingConfig{URL: env["EMBEDDING_URL"], Model: "bge-m3:567m", ModelDigest: digest, Dimensions: "1024", ContextLength: "8192", Timeout: "60s"})
	must(t, e)
	defer embed.Close()
	modelIdentity, e := embed.InspectIdentity(context.Background())
	must(t, e)
	if modelIdentity.Digest != digest {
		t.Fatal("model mismatch")
	}
	save(t, "model-identity", modelIdentity)
	api := start(t, "API", "api", env)
	client := &http.Client{Timeout: 45 * time.Second}
	until(t, "API health", 30, func() bool {
		r, e := client.Get(base + "/health")
		if e != nil {
			return false
		}
		r.Body.Close()
		return r.StatusCode == 200
	})
	requestNumber := 0
	request := func(method, path, token string, body any, want int) (map[string]any, string) {
		t.Helper()
		var b []byte
		if body != nil {
			b, e = json.Marshal(body)
			must(t, e)
		}
		req, e := http.NewRequest(method, base+path, bytes.NewReader(b))
		must(t, e)
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		r, e := client.Do(req)
		must(t, e)
		defer r.Body.Close()
		data, e := io.ReadAll(io.LimitReader(r.Body, 2<<20))
		must(t, e)
		requestNumber++
		save(t, fmt.Sprintf("http-%02d", requestNumber), map[string]any{"method": method, "path": path, "status": r.StatusCode, "trace_id": r.Header.Get("X-Trace-ID"), "body": json.RawMessage(data)})
		if r.StatusCode != want {
			t.Fatalf("%s %s status=%d want=%d body=%s", method, path, r.StatusCode, want, data)
		}
		var result map[string]any
		must(t, json.Unmarshal(data, &result))
		return result, r.Header.Get("X-Trace-ID")
	}
	request("GET", "/api/v1/knowledge/search?q=Synthetic", "", nil, 401)
	request("POST", "/api/v1/admin/knowledge/documents", userToken, map[string]any{}, 403)
	request("GET", "/api/v1/knowledge/search?q=Synthetic", userToken, nil, 503)
	request("GET", "/api/v1/knowledge/vector-search?q=Synthetic", userToken, nil, 503)
	content := "【虚构测试】Synthetic async indexing rule，仅用于隔离验收。"
	draft, _ := request("POST", "/api/v1/admin/knowledge/documents", adminToken, map[string]any{"document_key": "runtime-synthetic", "version": "test-v1", "category_id": category.ID, "title": "【虚构测试】异步发布链路", "content": content, "source_url": "https://www.bilibili.com/test-only", "effective_at": time.Now().UTC().Add(-time.Hour).Format(time.RFC3339), "articles": []any{map[string]any{"section_path": "虚构章节", "article_number": "1", "content": content}}}, 201)
	doc := uint64(draft["document_id"].(float64))
	version := uint64(draft["version_id"].(float64))
	docPath := fmt.Sprintf("/api/v1/admin/knowledge/documents/%d", doc)
	versionPath := fmt.Sprintf("/api/v1/admin/knowledge/versions/%d", version)
	detail, _ := request("GET", versionPath, adminToken, nil, 200)
	request("POST", versionPath+"/verifications", adminToken, map[string]any{"expected_content_revision": detail["content_revision"], "verification_note": "【虚构测试】隔离流程验收，不表示核实官方依据。"}, 201)
	state, _ := request("GET", docPath, adminToken, nil, 200)
	_, trace := request("POST", docPath+"/publish", adminToken, map[string]any{"version_id": version, "expected_publication_revision": state["publication_revision"]}, 200)
	var source model.EventOutbox
	must(t, db.Where("trace_id = ? AND topic = ?", trace, "knowledge.document.events").Take(&source).Error)
	if source.Status != model.OutboxPending {
		t.Fatal("stopped worker did not leave pending outbox")
	}
	save(t, "publish-pending", map[string]any{"event_id": source.EventID, "trace_id": trace, "status": source.Status})
	request("GET", docPath+"/deliveries", userToken, nil, 403)
	request("GET", docPath+"/deliveries", adminToken, nil, 200)
	active, e := repository.NewKnowledgeActiveRepository(db)
	must(t, e)
	worker := start(t, "WORKER", "worker-first", env)
	var generation model.KnowledgeIndexGeneration
	waitPublished := func(empty bool, old string) {
		t.Helper()
		until(t, "published generation", 180, func() bool {
			target, err := active.ResolveActive(context.Background())
			if err != nil || target.Empty != empty || target.GenerationID == old {
				return false
			}
			var inbox model.KnowledgeEventInbox
			if db.Where("event_id=?", source.EventID).Take(&inbox).Error != nil || inbox.Status != model.KnowledgeEventApplied {
				return false
			}
			if inbox.TraceID != source.TraceID {
				t.Fatal("inbox trace differs")
			}
			var loaded model.KnowledgeIndexGeneration
			must(t, db.Where("id=?", target.GenerationID).Take(&loaded).Error)
			generation = loaded
			if generation.EmbeddingDigest == nil || *generation.EmbeddingDigest != digest || generation.ESManifestSHA256 == nil || generation.MilvusManifestSHA256 == nil || *generation.ESManifestSHA256 != target.ManifestSHA256 || *generation.MilvusManifestSHA256 != target.ManifestSHA256 {
				t.Fatal("manifest or model mismatch")
			}
			return true
		})
		save(t, "generation-"+generation.ID, generation)
	}
	waitPublished(false, "")
	search := func(path string, want int) map[string]any {
		t.Helper()
		r, _ := request("GET", fmt.Sprintf("/api/v1/knowledge/%s?q=Synthetic&document_id=%d&limit=3", path, doc), userToken, nil, 200)
		if int(r["count"].(float64)) != want {
			t.Fatalf("%s count=%v want=%d", path, r["count"], want)
		}
		return r
	}
	es := search("search", 1)
	vec := search("vector-search", 1)
	esID := es["items"].([]any)[0].(map[string]any)["chunk_id"]
	vecID := vec["items"].([]any)[0].(map[string]any)["chunk_id"]
	if esID != vecID {
		t.Fatal("two indexes returned different chunk")
	}
	var attempt model.KnowledgeIndexAttempt
	must(t, db.Where("event_id=? AND status=?", source.EventID, model.KnowledgeAttemptSucceeded).Take(&attempt).Error)
	if attempt.GenerationID == nil || *attempt.GenerationID != generation.ID {
		t.Fatal("attempt generation mismatch")
	}
	save(t, "publish-correlation", map[string]any{"event_id": source.EventID, "trace_id": trace, "attempt": attempt, "chunk_id": esID, "generation_id": generation.ID})
	worker.stop(t)
	old := generation.ID
	state, _ = request("GET", docPath, adminToken, nil, 200)
	_, trace = request("POST", docPath+"/withdraw", adminToken, map[string]any{"version_id": version, "expected_publication_revision": state["publication_revision"]}, 200)
	source = model.EventOutbox{}
	must(t, db.Where("trace_id=? AND topic=?", trace, "knowledge.document.events").Take(&source).Error)
	if source.Status != model.OutboxPending {
		t.Fatal("withdrawal not pending while worker stopped")
	}
	target, e := active.ResolveActive(context.Background())
	must(t, e)
	if target.GenerationID != old {
		t.Fatal("active changed without worker")
	}
	search("search", 0)
	search("vector-search", 0) // MySQL authoritative filtering before the rebuild.
	worker = start(t, "WORKER", "worker-resumed", env)
	waitPublished(true, old)
	search("search", 0)
	search("vector-search", 0)
	request("GET", fmt.Sprintf("/api/v1/knowledge/versions/%d", version), userToken, nil, 200)
	request("GET", docPath+"/deliveries", adminToken, nil, 200)
	api.stop(t)
	api = start(t, "API", "api-restarted", env)
	until(t, "API restart with empty active", 30, func() bool {
		r, e := client.Get(base + "/health")
		if e != nil {
			return false
		}
		r.Body.Close()
		return r.StatusCode == 200
	})
	search("search", 0)
	if unexpectedLLM.Load() != 0 {
		t.Fatal("knowledge flow unexpectedly called conversation model")
	}
	worker.stop(t)
	api.stop(t)
	save(t, "runtime-result", map[string]any{"passed": true, "publish_chunk_id": esID, "last_generation": generation.ID, "last_empty": generation.Empty, "model_digest": digest, "restart": "actual child processes; forced stop, not graceful shutdown", "scope": "real API HTTP, Kafka, worker, MySQL, ES, Milvus and pinned embedding; conversational Agent inference not exercised"})
}
