//go:build publishing_integration

package router

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/kmsg"
	"go.uber.org/zap"
	"video-support-agent/internal/auth"
	"video-support-agent/internal/config"
	"video-support-agent/internal/eventbus"
	"video-support-agent/internal/model"
	"video-support-agent/internal/outboxrelay"
	"video-support-agent/internal/repository"
)

type recoveryStore struct {
	*repository.OutboxRepository
	cancel   context.CancelFunc
	failSent bool
}

func (s *recoveryStore) MarkSent(ctx context.Context, id, owner string) error {
	defer s.cancel()
	if s.failSent {
		return errors.New("injected bookkeeping unavailable")
	}
	return s.OutboxRepository.MarkSent(ctx, id, owner)
}
func (s *recoveryStore) MarkRetry(ctx context.Context, id, owner, code string, d time.Duration) error {
	defer s.cancel()
	return s.OutboxRepository.MarkRetry(ctx, id, owner, code, d)
}

type hiddenAcknowledgement struct{ eventbus.Publisher }

func (p hiddenAcknowledgement) Publish(ctx context.Context, r eventbus.Record) (eventbus.Receipt, error) {
	receipt, e := p.Publisher.Publish(ctx, r)
	if e != nil {
		return receipt, e
	}
	return eventbus.Receipt{}, eventbus.ErrDeliveryUnknown
}

func recoveryDocker(t *testing.T, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	b, e := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if e != nil {
		t.Fatalf("docker operation %s failed: %v %s", args[0], e, b)
	}
	return strings.TrimSpace(string(b))
}
func recoveryBroker(t *testing.T) string {
	t.Helper()
	id := os.Getenv("KAFKA_TEST_CONTAINER")
	project := os.Getenv("KAFKA_TEST_PROJECT")
	if id == "" || project == "" || !strings.HasPrefix(project, "publishing-recovery-") {
		t.Fatal("missing isolated broker ownership")
	}
	if recoveryDocker(t, "inspect", "--format", `{{index .Config.Labels "com.docker.compose.project"}}`, id) != project {
		t.Fatal("broker ownership mismatch")
	}
	return id
}
func recoveryClient(t *testing.T) (*eventbus.Client, *kgo.Client) {
	t.Helper()
	address := os.Getenv("KAFKA_TEST_ADDRESS")
	cluster := os.Getenv("KAFKA_TEST_CLUSTER")
	if address == "" || cluster == "" || address == "127.0.0.1:19092" {
		t.Fatal("isolated broker identity required")
	}
	cfg := config.KafkaConfig{Brokers: address, ClientID: "publishing-recovery", ConnectTimeout: "1s", RequestTimeout: "2s", DeliveryTimeout: "4s", ShutdownTimeout: "5s"}
	p, e := eventbus.NewClient(cfg)
	Must(t, e)
	t.Cleanup(func() {
		ctx, c := context.WithTimeout(context.Background(), 8*time.Second)
		defer c()
		if e := p.Close(ctx); e != nil {
			t.Errorf("producer close: %v", e)
		}
	})
	raw, e := kgo.NewClient(kgo.SeedBrokers(address), kgo.ConsumePartitions(map[string]map[int32]kgo.Offset{eventbus.TopicDocumentEvents: {0: kgo.NewOffset().AtStart(), 1: kgo.NewOffset().AtStart(), 2: kgo.NewOffset().AtStart()}}))
	Must(t, e)
	t.Cleanup(raw.Close)
	ctx, c := context.WithTimeout(context.Background(), 15*time.Second)
	defer c()
	metadata, e := kmsg.NewPtrMetadataRequest().RequestWith(ctx, raw)
	Must(t, e)
	if metadata.ClusterID == nil || *metadata.ClusterID != cluster {
		t.Fatal("cluster identity mismatch")
	}
	Must(t, p.CheckTopics(ctx))
	t.Logf("verified cluster=%s address=%s", cluster, address)
	return p, raw
}
func recoveryRow(t *testing.T, f *publicationFixture, id uint64) model.EventOutbox {
	t.Helper()
	var row model.EventOutbox
	Must(t, f.observer.Where("message_key=?", strconv.FormatUint(id, 10)).Order("id DESC").Take(&row).Error)
	return row
}
func recoveryWire(t *testing.T, raw *kgo.Client, row model.EventOutbox, count int) []*kgo.Record {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	var found []*kgo.Record
	for len(found) < count && ctx.Err() == nil {
		fetches := raw.PollFetches(ctx)
		if errs := fetches.Errors(); len(errs) > 0 {
			t.Fatalf("fetch errors: %v", errs)
		}
		fetches.EachRecord(func(r *kgo.Record) {
			headers := map[string]string{}
			for _, h := range r.Headers {
				headers[h.Key] = string(h.Value)
			}
			if headers["message_id"] != row.MessageID {
				return
			}
			if headers["trace_id"] != row.TraceID || !bytes.Equal(r.Value, row.Payload) || string(r.Key) != row.MessageKey {
				t.Fatal("wire bytes/key/trace changed")
			}
			found = append(found, r)
			t.Logf("wire message=%s event=%s partition=%d offset=%d bytes=%d", row.MessageID, row.EventID, r.Partition, r.Offset, len(r.Value))
		})
	}
	if len(found) < count {
		t.Fatalf("received %d want >=%d", len(found), count)
	}
	return found
}
func TestPublishingRelayRecoveryIntegration(t *testing.T) {
	f := newPublicationFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	f.ctx = ctx
	Must(t, f.observer.Exec("DELETE FROM event_outboxes").Error)
	producer, raw := recoveryClient(t)
	run := func(p eventbus.Publisher, fail bool) {
		c, stop := context.WithTimeout(ctx, 20*time.Second)
		defer stop()
		repo, e := repository.NewOutboxRepository(f.db)
		Must(t, e)
		relay, e := outboxrelay.New(&recoveryStore{repo, stop, fail}, p, zap.NewNop())
		Must(t, e)
		e = relay.Run(c)
		if !errors.Is(e, context.Canceled) {
			t.Fatalf("relay did not stop at settlement: %v", e)
		}
	}
	t.Run("AcknowledgedButUnrecorded", func(t *testing.T) {
		v := f.draft(t, nil, nil, false)
		row := recoveryRow(t, f, v.DocumentID)
		run(producer, true)
		saved := recoveryRow(t, f, v.DocumentID)
		if saved.Status != model.OutboxSending || saved.Attempts != 1 {
			t.Fatal("failed bookkeeping lost recovery state")
		}
		one := recoveryWire(t, raw, row, 1)
		// Accelerate only this isolated fixture's lease; production 90s constant is untouched.
		Must(t, f.observer.Exec("UPDATE event_outboxes SET lease_until=UTC_TIMESTAMP(3)-INTERVAL 1 SECOND WHERE id=?", row.ID).Error)
		run(producer, false)
		saved = recoveryRow(t, f, v.DocumentID)
		if saved.Status != model.OutboxSent || saved.Attempts != 2 || saved.OwnerToken != nil || saved.SentAt == nil {
			t.Fatal("recovery not recorded")
		}
		two := recoveryWire(t, raw, row, 1)
		if one[0].Partition != two[0].Partition || one[0].Offset >= two[0].Offset {
			t.Fatal("duplicate is not a later record on same partition")
		}
	})
	t.Run("AcknowledgementHidden", func(t *testing.T) {
		v := f.draft(t, nil, nil, false)
		row := recoveryRow(t, f, v.DocumentID)
		run(hiddenAcknowledgement{producer}, false)
		saved := recoveryRow(t, f, v.DocumentID)
		if saved.Status != model.OutboxPending || saved.SafeErrorCode != repository.OutboxDeliveryUnknown {
			t.Fatal("unknown result not retained")
		}
		recoveryWire(t, raw, row, 1)
		Must(t, f.observer.Exec("UPDATE event_outboxes SET next_attempt_at=UTC_TIMESTAMP(3)-INTERVAL 1 SECOND WHERE id=?", row.ID).Error)
		run(producer, false)
		recoveryWire(t, raw, row, 1)
		Equal(t, recoveryRow(t, f, v.DocumentID).Attempts, uint64(2))
	})
	t.Run("BrokerUnavailableThenRecovers", func(t *testing.T) {
		v := f.draft(t, nil, nil, false)
		row := recoveryRow(t, f, v.DocumentID)
		broker := recoveryBroker(t)
		recoveryDocker(t, "pause", broker)
		defer func() {
			if recoveryDocker(t, "inspect", "--format", "{{.State.Paused}}", broker) == "true" {
				recoveryDocker(t, "unpause", broker)
			}
		}()
		run(producer, false)
		saved := recoveryRow(t, f, v.DocumentID)
		if saved.Status != model.OutboxPending || saved.SafeErrorCode != repository.OutboxDeliveryUnknown {
			t.Fatal("broker outage lost pending event")
		}
		recoveryDocker(t, "unpause", broker)
		Must(t, f.observer.Exec("UPDATE event_outboxes SET next_attempt_at=UTC_TIMESTAMP(3)-INTERVAL 1 SECOND WHERE id=?", row.ID).Error)
		run(producer, false)
		Equal(t, recoveryRow(t, f, v.DocumentID).Status, model.OutboxSent)
		recoveryWire(t, raw, row, 1)
	})
}

func recoveryWorker(t *testing.T, name string) string {
	t.Helper()
	network := os.Getenv("KAFKA_TEST_PROJECT") + "_default"
	binary := os.Getenv("WORKER_TEST_BINARY")
	if binary == "" || !filepath.IsAbs(binary) || os.Getenv("WORKER_TEST_MYSQL_CONTAINER") == "" {
		t.Fatal("worker test environment missing")
	}
	name = os.Getenv("KAFKA_TEST_PROJECT") + "-" + name
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "run", "-d", "--name", name, "--label", "publishing.worker.owner="+os.Getenv("KAFKA_TEST_PROJECT"), "--network", network, "--mount", "type=bind,source="+binary+",target=/worker,readonly", "-e", "DB_HOST="+os.Getenv("WORKER_TEST_MYSQL_CONTAINER"), "-e", "DB_PORT=3306", "-e", "DB_USER", "-e", "DB_PASSWORD", "-e", "DB_NAME", "-e", "KAFKA_BROKERS=kafka:9092", "-e", "KAFKA_CONNECT_TIMEOUT=1s", "-e", "KAFKA_REQUEST_TIMEOUT=2s", "-e", "KAFKA_DELIVERY_TIMEOUT=4s", "-e", "KAFKA_SHUTDOWN_TIMEOUT=5s", "--entrypoint", "/worker", "gcr.io/distroless/static-debian12:nonroot")
	b, e := cmd.CombinedOutput()
	if e != nil {
		t.Fatalf("worker create: %v %s", e, b)
	}
	t.Cleanup(func() {
		logs := recoveryDocker(t, "logs", name)
		if secret := os.Getenv("DB_PASSWORD"); secret != "" && strings.Contains(logs, secret) {
			t.Error("worker credential leak")
			logs = strings.ReplaceAll(logs, secret, "[REDACTED]")
		}
		Must(t, os.WriteFile(filepath.Join(os.Getenv("WORKER_TEST_EVIDENCE"), name+".log"), []byte(logs), 0600))
		if strings.Contains(logs, "owner_token") || strings.Contains(logs, "payload_sha256") || strings.Contains(logs, "schema_version") {
			t.Error("worker leaked internal record")
		}
		if recoveryDocker(t, "inspect", "--format", `{{index .Config.Labels "publishing.worker.owner"}}`, name) != os.Getenv("KAFKA_TEST_PROJECT") {
			t.Fatal("worker ownership mismatch")
		}
		recoveryDocker(t, "rm", "-f", "-v", name)
	})
	return name
}
func recoveryAwait(t *testing.T, description string, condition func() bool) {
	t.Helper()
	until := time.Now().Add(20 * time.Second)
	for time.Now().Before(until) {
		if condition() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("timed out: " + description)
}
func recoveryExited(t *testing.T, name, code string) {
	t.Helper()
	recoveryAwait(t, "worker exit", func() bool { return recoveryDocker(t, "inspect", "--format", "{{.State.Running}}", name) == "false" })
	Equal(t, recoveryDocker(t, "inspect", "--format", "{{.State.ExitCode}}", name), code)
}
func TestPublishingWorkerProcessIntegration(t *testing.T) {
	f := newPublicationFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	f.ctx = ctx
	Must(t, f.observer.Exec("DELETE FROM event_outboxes").Error)
	_, raw := recoveryClient(t)
	broker := recoveryBroker(t)
	t.Run("MissingTableRejectsStartup", func(t *testing.T) {
		Must(t, f.observer.Exec("RENAME TABLE event_outboxes TO event_outboxes_test_hidden").Error)
		defer func() { Must(t, f.observer.Exec("RENAME TABLE event_outboxes_test_hidden TO event_outboxes").Error) }()
		w := recoveryWorker(t, "missing-table")
		recoveryExited(t, w, "1")
		if !strings.Contains(recoveryDocker(t, "logs", w), "outbox_schema_check_failed") {
			t.Fatal("missing safe schema error")
		}
	})
	t.Run("BadTopicRejectsStartup", func(t *testing.T) {
		alter := func(value string) {
			recoveryDocker(t, "exec", broker, "/opt/kafka/bin/kafka-configs.sh", "--bootstrap-server", "kafka:9092", "--entity-type", "topics", "--entity-name", eventbus.TopicDocumentEvents, "--alter", "--add-config", "retention.ms="+value)
		}
		alter("60000")
		defer alter("604800000")
		w := recoveryWorker(t, "bad-topic")
		recoveryExited(t, w, "1")
		if !strings.Contains(recoveryDocker(t, "logs", w), "kafka_topic_check_failed") {
			t.Fatal("missing safe topic error")
		}
	})
	t.Run("CrashRecoveryAndGracefulShutdown", func(t *testing.T) {
		w := recoveryWorker(t, "crash")
		recoveryAwait(t, "worker ready", func() bool { return strings.Contains(recoveryDocker(t, "logs", w), "worker started") })
		recoveryDocker(t, "pause", broker)
		defer func() {
			if recoveryDocker(t, "inspect", "--format", "{{.State.Paused}}", broker) == "true" {
				recoveryDocker(t, "unpause", broker)
			}
		}()
		// Exercise the real API composition while Kafka is unavailable.
		tokens, e := auth.NewTokenManager(strings.Repeat("r", 32), "recovery-test", time.Hour)
		Must(t, e)
		token, e := tokens.Issue(f.userID)
		Must(t, e)
		r := New(config.Config{AppName: "recovery-test"}, f.observer, tokens, nil, AllowLimiter{}, nil, zap.NewNop(), nil)
		input := Input()
		input.DocumentKey = f.prefix + "_http_recovery"
		f.keys = append(f.keys, input.DocumentKey)
		input.CategoryID = f.category
		response := Request(r, "POST", "/api/v1/admin/knowledge/documents", JSON(t, Payload(input)), token, false)
		Equal(t, response.Code, 201)
		var row model.EventOutbox
		Must(t, f.observer.Order("id DESC").Take(&row).Error)
		Equal(t, row.TraceID, response.Header().Get("X-Trace-ID"))
		recoveryAwait(t, "inflight claim", func() bool {
			var current model.EventOutbox
			Must(t, f.observer.First(&current, row.ID).Error)
			return current.Status == model.OutboxSending
		})
		recoveryDocker(t, "kill", "--signal", "KILL", w)
		recoveryExited(t, w, "137")
		var crashed model.EventOutbox
		Must(t, f.observer.First(&crashed, row.ID).Error)
		Equal(t, crashed.Status, model.OutboxSending)
		recoveryDocker(t, "unpause", broker)
		Must(t, f.observer.Exec("UPDATE event_outboxes SET lease_until=UTC_TIMESTAMP(3)-INTERVAL 1 SECOND WHERE id=?", row.ID).Error)
		restarted := recoveryWorker(t, "restarted")
		recoveryAwait(t, "recovered sent", func() bool {
			var current model.EventOutbox
			Must(t, f.observer.First(&current, row.ID).Error)
			return current.Status == model.OutboxSent
		})
		Must(t, f.observer.First(&crashed, row.ID).Error)
		if crashed.Attempts < 2 {
			t.Fatal("claim was not recovered")
		}
		recoveryWire(t, raw, row, 1)
		recoveryDocker(t, "kill", "--signal", "TERM", restarted)
		recoveryExited(t, restarted, "0")
		if !strings.Contains(recoveryDocker(t, "logs", restarted), "worker stopped") {
			t.Fatal("missing orderly shutdown")
		}
		// Committed events survive when the worker is stopped, then drain on restart.
		v := f.draft(t, nil, nil, false)
		pending := recoveryRow(t, f, v.DocumentID)
		Equal(t, pending.Status, model.OutboxPending)
		resumed := recoveryWorker(t, "backlog")
		recoveryAwait(t, "backlog sent", func() bool { return recoveryRow(t, f, v.DocumentID).Status == model.OutboxSent })
		recoveryWire(t, raw, pending, 1)
		recoveryDocker(t, "kill", "--signal", "TERM", resumed)
		recoveryExited(t, resumed, "0")
		t.Log("API committed with broker paused; SIGKILL left recoverable lease; new process delivered; SIGTERM exited 0; stopped-worker backlog survived")
	})
}
