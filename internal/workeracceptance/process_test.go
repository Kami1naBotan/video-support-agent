//go:build workerkafka

package workeracceptance

import (
	"context"
	"fmt"
	"github.com/twmb/franz-go/pkg/kgo"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"video-support-agent/internal/eventbus"
	"video-support-agent/internal/model"
)

func modelEndpoint(t *testing.T) (string, *atomic.Int32) {
	unexpected := &atomic.Int32{}
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/tags":
			fmt.Fprint(w, `{"models":[{"name":"bge-m3:567m","digest":"7907646426070047a77226ac3e684fbbe8410524f7b4a74d02837e43f2146bab"}]}`)
		case "/api/show":
			fmt.Fprint(w, `{"capabilities":["embedding"],"model_info":{"bert.embedding_length":1024,"bert.context_length":8192}}`)
		case "/api/version":
			fmt.Fprint(w, `{"version":"isolated-metadata-test"}`)
		default:
			unexpected.Add(1)
			http.Error(w, "unexpected indexing call", 500)
		}
	}))
	l, e := net.Listen("tcp", "0.0.0.0:0")
	must(t, e)
	s.Listener = l
	s.Start()
	t.Cleanup(s.Close)
	return "http://host.docker.internal:" + fmt.Sprint(l.Addr().(*net.TCPAddr).Port), unexpected
}
func process(t *testing.T, suffix, modelURL, sourceGroup, retryGroup string) string {
	project := os.Getenv("KAFKA_TEST_PROJECT")
	name := project + "-" + suffix
	binary := os.Getenv("WORKER_TEST_BINARY")
	if !filepath.IsAbs(binary) {
		t.Fatal("absolute binary required")
	}
	definition, e := filepath.Abs("../../deploy/elasticsearch/rule-chunks-v1.json")
	must(t, e)
	args := []string{"run", "-d", "--name", name, "--label", "publishing.worker.owner=" + project, "--network", project + "_default", "--mount", "type=bind,source=" + binary + ",target=/worker,readonly", "--mount", "type=bind,source=" + definition + ",target=/definition.json,readonly", "-e", "DB_HOST=" + os.Getenv("WORKER_TEST_MYSQL_CONTAINER"), "-e", "DB_PORT=3306", "-e", "DB_USER", "-e", "DB_PASSWORD", "-e", "DB_NAME", "-e", "KAFKA_BROKERS=kafka:9092", "-e", "KAFKA_CONNECT_TIMEOUT=1s", "-e", "KAFKA_REQUEST_TIMEOUT=2s", "-e", "KAFKA_DELIVERY_TIMEOUT=4s", "-e", "KAFKA_SHUTDOWN_TIMEOUT=5s", "-e", "KNOWLEDGE_SOURCE_SERVER_UUID=" + os.Getenv("TEST_MYSQL_SERVER_UUID"), "-e", "KNOWLEDGE_GENERATION_LIMIT=100", "-e", "KNOWLEDGE_ES_DEFINITION=/definition.json", "-e", "KNOWLEDGE_SOURCE_GROUP=" + sourceGroup, "-e", "KNOWLEDGE_RETRY_GROUP=" + retryGroup, "-e", "EMBEDDING_URL=" + modelURL, "-e", "ES_URL=" + modelURL, "-e", "MILVUS_URL=" + modelURL, "--entrypoint", "/worker", "gcr.io/distroless/static-debian12:nonroot"}
	ctx, c := context.WithTimeout(context.Background(), 30*time.Second)
	defer c()
	b, e := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if e != nil {
		t.Fatalf("worker create=%v %s", e, b)
	}
	t.Cleanup(func() {
		logs := docker(t, "logs", name)
		for _, secret := range []string{os.Getenv("DB_PASSWORD"), "sensitive-process-canary", "private-process-header"} {
			if secret != "" && strings.Contains(logs, secret) {
				t.Error("worker log leak")
				logs = strings.ReplaceAll(logs, secret, "[REDACTED]")
			}
		}
		must(t, os.WriteFile(filepath.Join(os.Getenv("WORKER_TEST_EVIDENCE"), suffix+".log"), []byte(logs), 0600))
		if docker(t, "inspect", "--format", `{{index .Config.Labels "publishing.worker.owner"}}`, name) != project {
			t.Fatal("worker owner mismatch")
		}
		docker(t, "rm", "-f", "-v", name)
	})
	return name
}
func exited(t *testing.T, name, code string) {
	await(t, "process exit", func() bool { return docker(t, "inspect", "--format", "{{.State.Running}}", name) == "false" })
	if got := docker(t, "inspect", "--format", "{{.State.ExitCode}}", name); got != code {
		t.Fatalf("exit=%s want=%s logs=%s", got, code, docker(t, "logs", name))
	}
}
func TestWorkerProcessKafka(t *testing.T) {
	db, repo := workerSQLFixture(t)
	cfg, _, raw := kafka(t)
	endpoint, unexpected := modelEndpoint(t)
	g := groupAtEnd(t, raw, eventbus.TopicDocumentEvents)
	retry := groupAtEnd(t, raw, eventbus.TopicIndexRetry)
	w := process(t, "crash", endpoint, g, retry)
	await(t, "worker started", func() bool { return strings.Contains(docker(t, "logs", w), "worker started") })
	await(t, "empty bootstrap published", func() bool { var c model.KnowledgeIndexControl; must(t, db.Take(&c).Error); return c.ActiveEpoch == 1 })
	b := broker(t)
	docker(t, "pause", b)
	defer func() {
		if docker(t, "inspect", "--format", "{{.State.Paused}}", b) == "true" {
			docker(t, "unpause", b)
		}
	}()
	must(t, repo.IsolatePoison(context.Background(), eventbus.Delivery{Topic: eventbus.TopicDocumentEvents, Partition: 0, Offset: 999, Value: []byte("synthetic crash trigger")}, "source_record_invalid"))
	var row model.EventOutbox
	must(t, db.Take(&row).Error)
	await(t, "inflight relay", func() bool { must(t, db.First(&row, row.ID).Error); return row.Status == model.OutboxSending })
	docker(t, "kill", "--signal", "KILL", w)
	exited(t, w, "137")
	must(t, db.First(&row, row.ID).Error)
	if row.Status != model.OutboxSending {
		t.Fatal("crash recovery state lost")
	}
	docker(t, "unpause", b)
	// Accelerate only the isolated persisted lease; do not alter production timeout.
	must(t, db.Exec("UPDATE event_outboxes SET lease_until=UTC_TIMESTAMP(3)-INTERVAL 1 SECOND WHERE id=?", row.ID).Error)
	resumed := process(t, "resumed", endpoint, g, retry)
	await(t, "outbox recovered", func() bool { must(t, db.First(&row, row.ID).Error); return row.Status == model.OutboxSent })
	if row.Attempts < 2 {
		t.Fatal("lease was not recovered")
	}
	wireRows(t, cfg.Brokers, []model.EventOutbox{row})
	bad := &kgo.Record{Topic: eventbus.TopicDocumentEvents, Value: []byte("sensitive-process-canary"), Headers: []kgo.RecordHeader{{Key: "secret", Value: []byte("private-process-header")}}}
	must(t, raw.ProduceSync(context.Background(), bad).FirstErr())
	awaitRebalance(t, func() bool { return committed(t, raw, g, bad.Topic, bad.Partition) == bad.Offset+1 })
	workerSQLCount(t, db, &model.KnowledgePoison{}, 2)
	docker(t, "kill", "--signal", "TERM", resumed)
	exited(t, resumed, "0")
	if !strings.Contains(docker(t, "logs", resumed), "worker stopped") {
		t.Fatal("missing orderly shutdown")
	}
	if unexpected.Load() != 0 {
		t.Fatal("empty lifecycle test invoked indexing endpoint")
	}
	t.Log("full seven-unit Linux worker: SIGKILL=137, recovered inflight lease, poison persisted before offset, SIGTERM=0; controlled model metadata only")
}

func awaitRebalance(t *testing.T, ready func() bool) {
	t.Helper()
	// SIGKILL cannot send LeaveGroup. Production session timeout is 45s and rebalance timeout 60s.
	end := time.Now().Add(75 * time.Second)
	for time.Now().Before(end) {
		if ready() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("consumer failed to recover after session/rebalance timeout")
}
