//go:build workerkafka

package workeracceptance

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"video-support-agent/internal/config"
	"video-support-agent/internal/handler"
	"video-support-agent/internal/middleware"
	"video-support-agent/internal/model"
	"video-support-agent/internal/observability"
	"video-support-agent/internal/repository"
	"video-support-agent/internal/service"
)

// One HTTP publication crosses the actual outbox/Kafka/worker/index path.
// Authentication is a fixture; handler, service, repository and Worker are real.
func TestWorkerObservationEndToEndKafka(t *testing.T) {
	db, _ := workerSQLFixture(t)
	_, _, _ = kafka(t)
	root, err := filepath.Abs("../..")
	require.NoError(t, err)
	evidence := os.Getenv("WORKER_TEST_EVIDENCE")
	project := os.Getenv("KAFKA_TEST_PROJECT") + "-e2e"
	run := func(timeout time.Duration, args ...string) string {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		out, e := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		require.NoError(t, e, "docker %s: %s", args[0], out)
		return strings.TrimSpace(string(out))
	}
	esFile := filepath.Join(evidence, "e2e-es.yaml")
	mvFile := filepath.Join(evidence, "e2e-mv.yaml")
	for _, item := range []struct {
		source, dest string
		swaps        []string
	}{
		{"compose.elasticsearch.yaml", esFile, []string{"127.0.0.1:9200:9200", "127.0.0.1::9200"}},
		{"compose.milvus.yaml", mvFile, []string{"127.0.0.1:19530:19530", "127.0.0.1::19530", "127.0.0.1:9091:9091", "127.0.0.1::9091"}},
	} {
		b, e := os.ReadFile(filepath.Join(root, item.source))
		require.NoError(t, e)
		require.NoError(t, os.WriteFile(item.dest, []byte(strings.NewReplacer(item.swaps...).Replace(string(b))), 0600))
	}
	compose := []string{"compose", "-p", project, "-f", esFile, "-f", mvFile}
	t.Cleanup(func() { run(time.Minute, append(compose, "down", "--volumes")...) })
	run(4*time.Minute, append(compose, "up", "-d", "--wait", "--wait-timeout", "210", "--pull", "never")...)
	esURL := "http://" + run(time.Second*10, append(compose, "port", "elasticsearch", "9200")...)
	mvURL := "http://" + run(time.Second*10, append(compose, "port", "standalone", "19530")...)
	jaegerArgs := []string{"compose", "-p", project + "-trace", "-f", filepath.Join(root, "compose.observability.yaml")}
	t.Cleanup(func() { run(time.Minute, append(jaegerArgs, "down", "--volumes")...) })
	run(time.Minute, append(jaegerArgs, "up", "-d", "--pull", "never", "jaeger")...)
	port := func() string {
		l, e := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, e)
		defer l.Close()
		return l.Addr().String()
	}
	startProcess := func(name string, args []string, extra map[string]string) *exec.Cmd {
		cmd := exec.Command(name, args...)
		cmd.Dir = root
		env := map[string]string{}
		for _, v := range os.Environ() {
			k, val, ok := strings.Cut(v, "=")
			if ok {
				env[k] = val
			}
		}
		for k, v := range extra {
			env[k] = v
		}
		for k, v := range env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		log, e := os.Create(filepath.Join(evidence, filepath.Base(name)+"-e2e.log"))
		require.NoError(t, e)
		cmd.Stdout = log
		cmd.Stderr = log
		require.NoError(t, cmd.Start())
		t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait(); _ = log.Close() })
		return cmd
	}
	embeddingAddr := port()
	startProcess("ollama", []string{"serve"}, map[string]string{"OLLAMA_HOST": embeddingAddr})
	client := &http.Client{Timeout: 5 * time.Second}
	require.Eventually(t, func() bool {
		r, e := client.Get("http://" + embeddingAddr + "/api/tags")
		if e != nil {
			return false
		}
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		return bytes.Contains(b, []byte("7907646426070047a77226ac3e684fbbe8410524f7b4a74d02837e43f2146bab"))
	}, 20*time.Second, 250*time.Millisecond)
	binary := filepath.Join(evidence, "worker-observed.exe")
	build := exec.Command("go", "build", "-o", binary, "./cmd/worker")
	build.Dir = root
	b, e := build.CombinedOutput()
	require.NoError(t, e, "build: %s", b)
	worker := startProcess(binary, nil, map[string]string{
		"KAFKA_BROKERS": os.Getenv("KAFKA_TEST_ADDRESS"), "KNOWLEDGE_SOURCE_SERVER_UUID": os.Getenv("TEST_MYSQL_SERVER_UUID"), "KNOWLEDGE_GENERATION_LIMIT": "100", "KNOWLEDGE_ES_DEFINITION": filepath.Join(root, "deploy/elasticsearch/rule-chunks-v1.json"),
		"KNOWLEDGE_SOURCE_GROUP": project + "-source", "KNOWLEDGE_RETRY_GROUP": project + "-retry", "ES_URL": esURL, "MILVUS_URL": mvURL, "EMBEDDING_URL": "http://" + embeddingAddr, "OBS_ENABLED": "true", "OBS_SERVICE_NAME": "e2e-observation", "OBS_OTLP_ENDPOINT": "http://127.0.0.1:4318", "OBS_SAMPLE_RATIO": "1", "OBS_METRICS_ADDR": port(),
	})
	_ = worker
	rt, e := observability.New(context.Background(), config.ObservabilityConfig{Enabled: "true", ServiceName: "e2e-observation", OTLPEndpoint: "http://127.0.0.1:4318", SampleRatio: "1", MetricsAddr: port()}, "api")
	require.NoError(t, e)
	defer rt.Shutdown(context.Background())
	mw, e := middleware.NewHTTPObservation(rt)
	require.NoError(t, e)
	user := model.User{Username: project, PasswordHash: "unused-test-hash", Role: model.RoleAdmin}
	require.NoError(t, db.Create(&user).Error)
	category := model.RuleCategory{Code: project, Name: "Synthetic E2E"}
	require.NoError(t, db.Create(&category).Error)
	h := handler.NewKnowledgeHandler(service.NewKnowledgeService(repository.NewKnowledgeRepository(db)))
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(mw, middleware.RequestTrace(), func(c *gin.Context) { c.Set("auth.user_id", user.ID); c.Next() })
	router.POST("/api/admin/knowledge/documents", h.Create)
	router.POST("/api/admin/knowledge/versions/:version_id/verifications", h.Verify)
	router.POST("/api/admin/knowledge/documents/:document_id/publish", h.Publish)
	server := httptest.NewServer(router)
	defer server.Close()
	post := func(path string, payload any, status int) map[string]any {
		b, e := json.Marshal(payload)
		require.NoError(t, e)
		r, e := client.Post(server.URL+path, "application/json", bytes.NewReader(b))
		require.NoError(t, e)
		defer r.Body.Close()
		raw, e := io.ReadAll(r.Body)
		require.NoError(t, e)
		require.Equal(t, status, r.StatusCode, "response: %s", raw)
		var out map[string]any
		require.NoError(t, json.Unmarshal(raw, &out))
		return out
	}
	created := post("/api/admin/knowledge/documents", map[string]any{"effective_at": time.Now().UTC().Add(-time.Minute).Format(time.RFC3339), "document_key": project, "version": "v1", "category_id": category.ID, "title": "Synthetic E2E", "content": "Synthetic observation rule.", "source_url": "https://www.bilibili.com/blackboard/test", "articles": []map[string]string{{"section_path": "test", "article_number": "1", "content": "Synthetic observation rule. 测试规则允许提交申诉材料。"}}}, 201)
	version := uint64(created["version_id"].(float64))
	doc := uint64(created["document_id"].(float64))
	post(fmt.Sprintf("/api/admin/knowledge/versions/%d/verifications", version), map[string]any{"expected_content_revision": 1, "verification_note": "Synthetic isolated acceptance"}, 201)
	post(fmt.Sprintf("/api/admin/knowledge/documents/%d/publish", doc), map[string]any{"version_id": version, "expected_publication_revision": 0}, 200)
	var inbox model.KnowledgeEventInbox
	require.Eventually(t, func() bool {
		return db.Where("document_id = ? AND status = ?", doc, model.KnowledgeEventApplied).Take(&inbox).Error == nil
	}, 150*time.Second, 250*time.Millisecond)
	var outbox model.EventOutbox
	require.NoError(t, db.Where("event_id = ?", inbox.EventID).Take(&outbox).Error)
	require.Equal(t, model.OutboxSent, outbox.Status)
	require.Len(t, outbox.TraceParent, 55)
	require.Len(t, inbox.TraceParent, 55)
	traceID := strings.Split(outbox.TraceParent, "-")[1]
	require.Equal(t, traceID, strings.Split(inbox.TraceParent, "-")[1])
	var attempt model.KnowledgeIndexAttempt
	require.NoError(t, db.Where("event_id = ?", inbox.EventID).Take(&attempt).Error)
	require.NotNil(t, attempt.GenerationID)
	var members []model.KnowledgeBuildEvent
	require.NoError(t, db.Where("generation_id = ?", *attempt.GenerationID).Find(&members).Error)
	metadata, _ := json.Marshal(map[string]any{"inbox": inbox.TraceParent, "attempt": attempt.TraceParent, "generation": *attempt.GenerationID, "members": members})
	require.NoError(t, os.WriteFile(filepath.Join(evidence, "e2e-members.json"), metadata, 0600))
	active, e := repository.NewKnowledgeActiveRepository(db)
	require.NoError(t, e)
	target, e := active.ResolveActive(context.Background())
	require.NoError(t, e)
	require.Equal(t, *attempt.GenerationID, target.GenerationID)
	fetch := func(path string) []byte {
		r, e := client.Get("http://127.0.0.1:16686" + path)
		if e != nil {
			return nil
		}
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		return b
	}
	require.Eventually(t, func() bool {
		b := fetch("/api/traces/" + traceID)
		if !bytes.Contains(b, []byte("knowledge.publish")) || !bytes.Contains(b, []byte("knowledge.receive")) || !bytes.Contains(b, []byte("POST /api/admin/knowledge/documents/:document_id/publish")) {
			return false
		}
		require.NoError(t, os.WriteFile(filepath.Join(evidence, "e2e-source-trace.json"), b, 0600))
		return true
	}, 20*time.Second, 250*time.Millisecond)
	require.Eventually(t, func() bool {
		b := fetch("/api/traces?service=e2e-observation-worker&operation=knowledge.build&limit=20")
		require.NoError(t, os.WriteFile(filepath.Join(evidence, "e2e-build-traces-latest.json"), b, 0600))
		var response struct {
			Data []struct {
				TraceID string `json:"traceID"`
				Spans   []struct {
					Operation  string `json:"operationName"`
					References []struct {
						TraceID string `json:"traceID"`
						SpanID  string `json:"spanID"`
					} `json:"references"`
					Tags []struct {
						Key   string `json:"key"`
						Value any    `json:"value"`
					} `json:"tags"`
				} `json:"spans"`
			} `json:"data"`
		}
		if json.Unmarshal(b, &response) != nil {
			return false
		}
		matched := false
		for _, tr := range response.Data {
			for _, span := range tr.Spans {
				if span.Operation != "knowledge.build" || tr.TraceID == traceID {
					continue
				}
				linked, published, identity := false, false, false
				for _, ref := range span.References {
					if ref.TraceID == traceID && ref.SpanID == strings.Split(inbox.TraceParent, "-")[2] {
						linked = true
					}
				}
				for _, tag := range span.Tags {
					if tag.Key == "app.generation_id" && tag.Value == target.GenerationID {
						identity = true
					}
					if tag.Key == "app.outcome" && tag.Value == "published" {
						published = true
					}
				}
				member := false
				for _, m := range members {
					if m.EventID == inbox.EventID {
						member = true
					}
				}
				// A late delivery can be settled against an already running build.
				// Require links for actual claim members; otherwise correlate by
				// immutable generation identity without inventing a future cause.
				matched = matched || identity && published && (!member || linked)
			}
		}
		if !matched {
			return false
		}
		require.NoError(t, os.WriteFile(filepath.Join(evidence, "e2e-build-traces.json"), b, 0600))
		return true
	}, 20*time.Second, 250*time.Millisecond)
	t.Logf("HTTP publish -> durable Outbox -> Kafka -> independent Worker PID=%d -> pinned real embedding -> dual index published -> Jaeger source=%s, generation=%s", worker.Process.Pid, traceID, target.GenerationID)
}
