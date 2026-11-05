//go:build workerkafka || workerdual

package workeracceptance

import (
	"context"
	"errors"
	driver "github.com/go-sql-driver/mysql"
	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/kmsg"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
	"video-support-agent/internal/config"
	"video-support-agent/internal/eventbus"
	"video-support-agent/internal/knowledgeevent"
	"video-support-agent/internal/model"
	"video-support-agent/internal/repository"
)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func await(t *testing.T, name string, condition func() bool) {
	t.Helper()
	end := time.Now().Add(30 * time.Second)
	for time.Now().Before(end) {
		if condition() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("timeout: " + name)
}
func docker(t *testing.T, args ...string) string {
	t.Helper()
	ctx, c := context.WithTimeout(context.Background(), 45*time.Second)
	defer c()
	b, e := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if e != nil {
		t.Fatalf("docker %s failed: %v %s", args[0], e, b)
	}
	return strings.TrimSpace(string(b))
}
func broker(t *testing.T) string {
	id := os.Getenv("KAFKA_TEST_CONTAINER")
	project := os.Getenv("KAFKA_TEST_PROJECT")
	if id == "" || !strings.HasPrefix(project, "worker-kafka-") {
		t.Fatal("isolated broker required")
	}
	if docker(t, "inspect", "--format", `{{index .Config.Labels "com.docker.compose.project"}}`, id) != project {
		t.Fatal("broker owner mismatch")
	}
	return id
}
func kafka(t *testing.T) (config.KafkaConfig, *eventbus.Client, *kgo.Client) {
	broker(t)
	cfg := config.KafkaConfig{Brokers: os.Getenv("KAFKA_TEST_ADDRESS"), ClientID: "worker-acceptance", ConnectTimeout: "1s", RequestTimeout: "2s", DeliveryTimeout: "4s", ShutdownTimeout: "5s"}
	p, e := eventbus.NewClient(cfg)
	must(t, e)
	t.Cleanup(func() { must(t, p.Close(context.Background())) })
	raw, e := kgo.NewClient(kgo.SeedBrokers(cfg.Brokers))
	must(t, e)
	t.Cleanup(raw.Close)
	ctx, c := context.WithTimeout(context.Background(), 10*time.Second)
	defer c()
	m, e := kmsg.NewPtrMetadataRequest().RequestWith(ctx, raw)
	must(t, e)
	if m.ClusterID == nil || *m.ClusterID != os.Getenv("KAFKA_TEST_CLUSTER") {
		t.Fatal("cluster mismatch")
	}
	must(t, p.CheckTopics(ctx))
	return cfg, p, raw
}
func groupAtEnd(t *testing.T, raw *kgo.Client, topic string) string {
	id, e := knowledgeevent.NewID()
	must(t, e)
	g := "worker-" + id
	ctx, c := context.WithTimeout(context.Background(), 10*time.Second)
	defer c()
	a := kadm.NewClient(raw)
	ends, e := a.ListEndOffsets(ctx, topic)
	must(t, e)
	must(t, ends.Error())
	o := kadm.Offsets{}
	for partition, r := range ends[topic] {
		o.AddOffset(topic, partition, r.Offset, -1)
	}
	response, e := a.CommitOffsets(ctx, g, o)
	must(t, e)
	must(t, response.Error())
	return g
}
func committed(t *testing.T, raw *kgo.Client, g, topic string, partition int32) int64 {
	t.Helper()
	ctx, c := context.WithTimeout(context.Background(), 5*time.Second)
	defer c()
	r, e := kadm.NewClient(raw).FetchOffsetsForTopics(ctx, g, topic)
	must(t, e)
	v, ok := r.Lookup(topic, partition)
	if !ok {
		t.Fatal("missing offset")
	}
	must(t, v.Err)
	return v.At
}
func startConsumer(t *testing.T, cfg config.KafkaConfig, g, topic string, h eventbus.DurableHandler) (context.CancelFunc, <-chan error) {
	c, e := eventbus.NewConsumer(cfg, g, topic)
	must(t, e)
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx, h) }()
	t.Cleanup(stop)
	return stop, done
}
func stopConsumer(t *testing.T, stop context.CancelFunc, done <-chan error) {
	t.Helper()
	stop()
	select {
	case e := <-done:
		if !errors.Is(e, context.Canceled) {
			t.Fatalf("consumer exit=%v", e)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("consumer did not stop")
	}
}
func wireSource(t *testing.T, p *eventbus.Client, s knowledgeevent.Source) eventbus.Receipt {
	d := workerSQLDelivery(t, s)
	r, e := p.Publish(context.Background(), eventbus.Record{Topic: d.Topic, Key: string(d.Key), Value: d.Value, MessageID: s.EventID, TraceID: s.TraceID})
	must(t, e)
	return r
}
func workerSQLMust(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func workerSQLFixture(t *testing.T) (*gorm.DB, *repository.KnowledgeBuildRepository) {
	t.Helper()
	if os.Getenv("TEST_ISOLATED") != "1" || os.Getenv("DB_HOST") != "127.0.0.1" || os.Getenv("DB_NAME") != "bili_support_test" || os.Getenv("TEST_MYSQL_SERVER_UUID") == "" {
		t.Fatal("dedicated isolated script required")
	}
	cfg := driver.NewConfig()
	cfg.Net = "tcp"
	cfg.Addr = net.JoinHostPort(os.Getenv("DB_HOST"), os.Getenv("DB_PORT"))
	cfg.User = os.Getenv("DB_USER")
	cfg.Passwd = os.Getenv("DB_PASSWORD")
	cfg.DBName = os.Getenv("DB_NAME")
	cfg.ParseTime = true
	cfg.Loc = time.UTC
	cfg.Timeout = 5 * time.Second
	cfg.ReadTimeout = 10 * time.Second
	cfg.WriteTimeout = 10 * time.Second
	db, err := gorm.Open(mysql.Open(cfg.FormatDSN()), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	workerSQLMust(t, err)
	pool, err := db.DB()
	workerSQLMust(t, err)
	t.Cleanup(func() { pool.Close() })
	var identity struct{ Name, UUID string }
	workerSQLMust(t, db.Raw("SELECT DATABASE() AS name, @@server_uuid AS uuid").Scan(&identity).Error)
	if identity.Name != "bili_support_test" || identity.UUID != os.Getenv("TEST_MYSQL_SERVER_UUID") {
		t.Fatal("database identity mismatch")
	}
	for _, value := range []any{&model.KnowledgeBuildEvent{}, &model.KnowledgeRetryReceipt{}, &model.KnowledgeBuildRecovery{}, &model.KnowledgeIndexAttempt{}, &model.KnowledgeEventInbox{}, &model.KnowledgeDocumentIndex{}, &model.KnowledgePoison{}, &model.EventOutbox{}, &model.KnowledgeIndexTask{}, &model.KnowledgeIndexGeneration{}} {
		workerSQLMust(t, db.Session(&gorm.Session{AllowGlobalUpdate: true}).Delete(value).Error)
	}
	workerSQLMust(t, db.Model(&model.KnowledgeIndexControl{}).Where("id = 1").Updates(map[string]any{"desired_epoch": 1, "active_epoch": 0, "active_generation_id": nil, "owner_token": nil, "lease_until": nil, "generation_limit": 100}).Error)
	workerSQLMust(t, db.Model(&model.KnowledgeIndexBootstrap{}).Where("id = 1").Updates(map[string]any{"status": "pending", "completed_at": nil}).Error)
	workerSQLMust(t, db.Model(&model.KnowledgeDocument{}).Where("current_version_id IS NOT NULL").Update("current_version_id", nil).Error)
	repo, err := repository.NewKnowledgeBuildRepository(db, 100)
	workerSQLMust(t, err)
	return db, repo
}

func workerSQLSource(t *testing.T, db *gorm.DB) knowledgeevent.Source {
	t.Helper()
	id, err := knowledgeevent.NewID()
	workerSQLMust(t, err)
	category := model.RuleCategory{Code: id, Name: "synthetic worker test"}
	workerSQLMust(t, db.Create(&category).Error)
	doc := model.KnowledgeDocument{DocumentKey: id, PublicationRevision: 1}
	workerSQLMust(t, db.Create(&doc).Error)
	now := time.Now().UTC().Truncate(time.Millisecond)
	version := model.RuleVersion{DocumentID: doc.ID, CategoryID: category.ID, Version: "test", ContentRevision: 1, Status: model.RuleVersionStatusPublished, PublishedAt: &now, EffectiveAt: &now, Title: "synthetic", Content: "test", SourceURL: "https://example.invalid/test"}
	workerSQLMust(t, db.Omit("Document", "Category").Create(&version).Error)
	workerSQLMust(t, db.Model(&doc).Update("current_version_id", version.ID).Error)
	return knowledgeevent.Source{SchemaVersion: 1, EventID: id, EventType: knowledgeevent.VersionPublished, DocumentID: strconv.FormatUint(doc.ID, 10), VersionID: strconv.FormatUint(version.ID, 10), ContentRevision: "1", PublicationRevision: "1", OccurredAt: now.Format(time.RFC3339Nano), TraceID: "1234567890abcdef1234567890abcdef"}
}
func workerSQLDelivery(t *testing.T, s knowledgeevent.Source) eventbus.Delivery {
	t.Helper()
	b, err := knowledgeevent.Encode(s)
	workerSQLMust(t, err)
	return eventbus.Delivery{Topic: eventbus.TopicDocumentEvents, Key: []byte(s.DocumentID), Value: b, Headers: []eventbus.Header{{Key: "message_id", Value: []byte(s.EventID)}, {Key: "trace_id", Value: []byte(s.TraceID)}}}
}
func workerSQLCount(t *testing.T, db *gorm.DB, value any, want int64) {
	t.Helper()
	var count int64
	workerSQLMust(t, db.Model(value).Count(&count).Error)
	if count != want {
		t.Fatalf("%T count=%d want=%d", value, count, want)
	}
}
