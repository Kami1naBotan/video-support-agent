//go:build workermysql

package repository

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"video-support-agent/internal/embedding"
	"video-support-agent/internal/eventbus"
	"video-support-agent/internal/knowledgeevent"
	"video-support-agent/internal/knowledgefailure"
	"video-support-agent/internal/knowledgeworker"
	"video-support-agent/internal/model"
)

func workerSQLMust(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func workerSQLFixture(t *testing.T) (*gorm.DB, *KnowledgeBuildRepository) {
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
	repo, err := NewKnowledgeBuildRepository(db, 100)
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

func TestWorkerSourceRecoveryMySQL(t *testing.T) {
	db, repo := workerSQLFixture(t)
	ctx := context.Background()
	inbox, err := NewKnowledgeInboxRepository(db)
	workerSQLMust(t, err)
	source := workerSQLSource(t, db)
	delivery := workerSQLDelivery(t, source)
	workerSQLMust(t, inbox.AcceptSource(ctx, delivery))
	workerSQLMust(t, inbox.AcceptSource(ctx, delivery))
	conflict := source
	conflict.TraceID = "abcdef1234567890abcdef1234567890"
	if err := inbox.AcceptSource(ctx, workerSQLDelivery(t, conflict)); !errors.Is(err, knowledgeworker.ErrSourceConflict) {
		t.Fatalf("conflict=%v", err)
	}
	for i := 1; i < 65; i++ {
		source.EventID, err = knowledgeevent.NewID()
		workerSQLMust(t, err)
		workerSQLMust(t, inbox.AcceptSource(ctx, workerSQLDelivery(t, source)))
	}
	workerSQLCount(t, db, &model.KnowledgeEventInbox{}, 65)
	workerSQLCount(t, db, &model.KnowledgeIndexAttempt{}, 65)
	claim, err := repo.ClaimLatest(ctx)
	workerSQLMust(t, err)
	if claim == nil {
		t.Fatal("no claim")
	}
	workerSQLCount(t, db, &model.KnowledgeBuildEvent{}, 65)
	workerSQLMust(t, repo.RecordBuildFailure(ctx, *claim, knowledgefailure.CodeDualExternalFailed))
	var first model.KnowledgeBuildRecovery
	workerSQLMust(t, db.Take(&first).Error)
	workerSQLMust(t, repo.RecordBuildFailure(ctx, *claim, knowledgefailure.CodeDualExternalFailed))
	var second model.KnowledgeBuildRecovery
	workerSQLMust(t, db.Take(&second).Error)
	if first.JitterPermille != second.JitterPermille || !first.NotBefore.Equal(*second.NotBefore) {
		t.Fatal("decision changed on replay")
	}
	var task model.KnowledgeIndexTask
	workerSQLMust(t, db.Take(&task).Error)
	if task.FailedAttempts != 1 {
		t.Fatal("failure counted twice")
	}
	blocked, err := repo.ClaimLatest(ctx)
	workerSQLMust(t, err)
	if blocked != nil {
		t.Fatal("pending recovery bypassed")
	}
	n, err := repo.RecoverPending(ctx)
	workerSQLMust(t, err)
	if n != 64 {
		t.Fatalf("first batch=%d", n)
	}
	workerSQLMust(t, db.Take(&second).Error)
	if second.Status != model.KnowledgeRecoveryPending {
		t.Fatal("recovery completed too early")
	}
	n, err = repo.RecoverPending(ctx)
	workerSQLMust(t, err)
	if n != 2 {
		t.Fatalf("second batch=%d", n)
	}
	workerSQLCount(t, db, &model.EventOutbox{}, 65)
	n, err = repo.RecoverPending(ctx)
	workerSQLMust(t, err)
	if n != 0 {
		t.Fatal("recovery duplicated")
	}
	var rows []model.EventOutbox
	workerSQLMust(t, db.Order("id").Find(&rows).Error)
	for _, row := range rows {
		d := eventbus.Delivery{Topic: row.Topic, Key: []byte(row.MessageKey), Value: row.Payload, Headers: []eventbus.Header{{Key: "message_id", Value: []byte(row.MessageID)}, {Key: "trace_id", Value: []byte(row.TraceID)}}}
		workerSQLMust(t, repo.AcceptRetry(ctx, d))
		d.Offset = 99
		workerSQLMust(t, repo.AcceptRetry(ctx, d))
	}
	workerSQLCount(t, db, &model.KnowledgeRetryReceipt{}, 65)
	workerSQLCount(t, db, &model.KnowledgeIndexAttempt{}, 130)
	now, err := buildDatabaseTime(db)
	workerSQLMust(t, err)
	if now.Before(*first.NotBefore) {
		early, err := repo.ClaimLatest(ctx)
		workerSQLMust(t, err)
		if early != nil {
			t.Fatal("retry ran early")
		}
	}
	if wait := time.Until(*first.NotBefore) + 50*time.Millisecond; wait > 0 {
		if wait > 7*time.Second {
			t.Fatal("unexpected backoff")
		}
		time.Sleep(wait)
	}
	next, err := repo.ClaimLatest(ctx)
	workerSQLMust(t, err)
	if next == nil || next.Attempt != 2 || next.GenerationID == claim.GenerationID || next.ESIndexName == claim.ESIndexName || next.MilvusCollection == claim.MilvusCollection {
		t.Fatalf("invalid retry claim: %+v", next)
	}
	snapshot, err := repo.Observe(ctx)
	workerSQLMust(t, err)
	if len(snapshot.Attempts) != 32 {
		t.Fatalf("sample limit=%d", len(snapshot.Attempts))
	}
}

func TestWorkerPoisonAtomicMySQL(t *testing.T) {
	db, repo := workerSQLFixture(t)
	ctx := context.Background()
	d := eventbus.Delivery{Topic: eventbus.TopicDocumentEvents, Partition: 1, Offset: 5, Value: []byte("sensitive malformed body")}
	workerSQLMust(t, db.Callback().Create().Before("gorm:create").Register("test:outbox_failure", func(tx *gorm.DB) {
		if tx.Statement.Schema != nil && tx.Statement.Schema.Table == "event_outboxes" {
			tx.AddError(errors.New("injected outbox failure"))
		}
	}))
	err := repo.IsolatePoison(ctx, d, "source_record_invalid")
	workerSQLMust(t, db.Callback().Create().Remove("test:outbox_failure"))
	if !errors.Is(err, knowledgeworker.ErrPoisonStore) {
		t.Fatalf("rollback err=%v", err)
	}
	workerSQLCount(t, db, &model.KnowledgePoison{}, 0)
	workerSQLCount(t, db, &model.EventOutbox{}, 0)
	workerSQLMust(t, repo.IsolatePoison(ctx, d, "source_record_invalid"))
	workerSQLMust(t, repo.IsolatePoison(ctx, d, "source_record_invalid"))
	workerSQLCount(t, db, &model.KnowledgePoison{}, 1)
	workerSQLCount(t, db, &model.EventOutbox{}, 1)
	d.Value = []byte("different")
	if err := repo.IsolatePoison(ctx, d, "source_record_invalid"); !errors.Is(err, knowledgeworker.ErrPoisonConflict) {
		t.Fatalf("conflict=%v", err)
	}
	workerSQLCount(t, db, &model.KnowledgeEventInbox{}, 0)
}

func TestWorkerClaimCompetitionMySQL(t *testing.T) {
	db, repo := workerSQLFixture(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	claims := make(chan *knowledgeworker.BuildClaim, 8)
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); c, e := repo.ClaimLatest(ctx); claims <- c; errs <- e }()
	}
	wg.Wait()
	close(claims)
	close(errs)
	for err := range errs {
		workerSQLMust(t, err)
	}
	var owner *knowledgeworker.BuildClaim
	count := 0
	for c := range claims {
		if c != nil {
			owner = c
			count++
		}
	}
	if count != 1 {
		t.Fatalf("owners=%d", count)
	}
	workerSQLMust(t, db.Model(&model.KnowledgeIndexControl{}).Where("id = 1").Update("lease_until", gorm.Expr("TIMESTAMPADD(SECOND,-1,UTC_TIMESTAMP(3))")).Error)
	next, err := repo.ClaimLatest(ctx)
	workerSQLMust(t, err)
	if next == nil || next.GenerationID == owner.GenerationID || next.OwnerToken == owner.OwnerToken || next.Attempt != owner.Attempt {
		t.Fatalf("takeover=%+v", next)
	}
	if err := repo.Renew(ctx, *owner); !errors.Is(err, knowledgeworker.ErrBuildLeaseLost) {
		t.Fatalf("old owner renew=%v", err)
	}
	workerSQLMust(t, db.Model(&model.KnowledgeIndexControl{}).Where("id = 1").Update("desired_epoch", 2).Error)
	if err := repo.Renew(ctx, *next); !errors.Is(err, knowledgeworker.ErrBuildObsolete) {
		t.Fatalf("old epoch renew=%v", err)
	}
}

type workerCommitPool struct{ *sql.DB }

func (p workerCommitPool) BeginTx(ctx context.Context, opts *sql.TxOptions) (gorm.ConnPool, error) {
	tx, err := p.DB.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &workerCommitTx{tx}, nil
}

type workerCommitTx struct{ *sql.Tx }

func (tx workerCommitTx) Commit() error {
	if err := tx.Tx.Commit(); err != nil {
		return err
	}
	return errors.New("injected commit acknowledgement loss")
}

func TestWorkerCommitUnknownMySQL(t *testing.T) {
	db, repo := workerSQLFixture(t)
	ctx := context.Background()
	claim, err := repo.ClaimLatest(ctx)
	workerSQLMust(t, err)
	if claim == nil {
		t.Fatal("missing claim")
	}
	pool, err := db.DB()
	workerSQLMust(t, err)
	wrapped, err := gorm.Open(mysql.New(mysql.Config{Conn: workerCommitPool{pool}, SkipInitializeWithVersion: true}), &gorm.Config{DisableAutomaticPing: true, Logger: logger.Default.LogMode(logger.Silent)})
	workerSQLMust(t, err)
	uncertain, err := NewKnowledgeBuildRepository(wrapped, 100)
	workerSQLMust(t, err)
	if err := uncertain.RecordBuildFailure(ctx, *claim, knowledgefailure.CodeDualExternalFailed); !errors.Is(err, knowledgeworker.ErrBuildStore) {
		t.Fatalf("unknown=%v", err)
	}
	var saved model.KnowledgeBuildRecovery
	workerSQLMust(t, db.Take(&saved).Error)
	workerSQLMust(t, repo.RecordBuildFailure(ctx, *claim, knowledgefailure.CodeDualExternalFailed))
	var task model.KnowledgeIndexTask
	workerSQLMust(t, db.Take(&task).Error)
	if task.FailedAttempts != 1 {
		t.Fatal("duplicate failure count")
	}
	n, err := uncertain.RecoverPending(ctx)
	if !errors.Is(err, knowledgeworker.ErrBuildStore) || n != 0 {
		t.Fatalf("unknown batch n=%d err=%v", n, err)
	}
	workerSQLMust(t, db.Take(&saved).Error)
	if saved.Status != model.KnowledgeRecoveryCompleted {
		t.Fatal("real commit missing")
	}
	n, err = repo.RecoverPending(ctx)
	workerSQLMust(t, err)
	if n != 0 {
		t.Fatal("repeated completion")
	}
	t.Log("real MySQL commit visible through normal pool; injected acknowledgement error reported to caller")
}

func TestWorkerTerminalRollbackMySQL(t *testing.T) {
	db, repo := workerSQLFixture(t)
	ctx := context.Background()
	inbox, err := NewKnowledgeInboxRepository(db)
	workerSQLMust(t, err)
	source := workerSQLSource(t, db)
	delivery := workerSQLDelivery(t, source)
	workerSQLMust(t, inbox.AcceptSource(ctx, delivery))
	claim, err := repo.ClaimLatest(ctx)
	workerSQLMust(t, err)
	if claim == nil {
		t.Fatal("no claim")
	}
	workerSQLMust(t, repo.RecordBuildFailure(ctx, *claim, knowledgefailure.CodeDualInputInvalid))
	workerSQLMust(t, db.Callback().Create().Before("gorm:create").Register("test:dlq_failure", func(tx *gorm.DB) {
		if tx.Statement.Schema != nil && tx.Statement.Schema.Table == "event_outboxes" {
			if row, ok := tx.Statement.Dest.(*model.EventOutbox); ok && row.Topic == eventbus.TopicIndexDLQ {
				tx.AddError(errors.New("injected DLQ outbox failure"))
			}
		}
	}))
	_, err = repo.RecoverPending(ctx)
	workerSQLMust(t, db.Callback().Create().Remove("test:dlq_failure"))
	if !errors.Is(err, knowledgeworker.ErrBuildStore) {
		t.Fatalf("rollback=%v", err)
	}
	workerSQLCount(t, db, &model.EventOutbox{}, 0)
	var event model.KnowledgeEventInbox
	workerSQLMust(t, db.Take(&event).Error)
	if event.TerminalMessageID != nil {
		t.Fatal("terminal escaped rollback")
	}
	var member model.KnowledgeBuildEvent
	workerSQLMust(t, db.Take(&member).Error)
	if member.RecoveryProcessedAt != nil {
		t.Fatal("processed marker escaped rollback")
	}
	n, err := repo.RecoverPending(ctx)
	workerSQLMust(t, err)
	if n != 2 {
		t.Fatalf("work=%d", n)
	}
	workerSQLCount(t, db, &model.EventOutbox{}, 2)
	workerSQLMust(t, db.Take(&event).Error)
	if event.Status != model.KnowledgeEventDeadLettered || event.ExpectedAttempt != 1 || event.TerminalMessageID == nil {
		t.Fatal("invalid deadletter terminal")
	}
	workerSQLMust(t, inbox.AcceptSource(ctx, delivery))
	n, err = repo.SettleReady(ctx)
	workerSQLMust(t, err)
	if n != 0 {
		t.Fatal("terminal resurrected")
	}
	workerSQLCount(t, db, &model.EventOutbox{}, 2)
}

func TestWorkerSourceOrderingMySQL(t *testing.T) {
	db, repo := workerSQLFixture(t)
	ctx := context.Background()
	inbox, err := NewKnowledgeInboxRepository(db)
	workerSQLMust(t, err)
	source := workerSQLSource(t, db)
	future := source
	future.PublicationRevision = "2"
	if err := inbox.AcceptSource(ctx, workerSQLDelivery(t, future)); !errors.Is(err, knowledgeworker.ErrSourceState) {
		t.Fatalf("future=%v", err)
	}
	workerSQLCount(t, db, &model.KnowledgeEventInbox{}, 0)
	workerSQLMust(t, inbox.AcceptSource(ctx, workerSQLDelivery(t, source)))
	workerSQLMust(t, db.Model(&model.KnowledgeDocument{}).Where("id = ?", source.DocumentID).Update("publication_revision", 2).Error)
	n, err := repo.SettleReady(ctx)
	workerSQLMust(t, err)
	if n != 1 {
		t.Fatalf("settle=%d", n)
	}
	var event model.KnowledgeEventInbox
	workerSQLMust(t, db.Take(&event).Error)
	if event.Status != model.KnowledgeEventSuperseded {
		t.Fatal("old event not superseded")
	}
	draft := source
	draft.EventID, err = knowledgeevent.NewID()
	workerSQLMust(t, err)
	draft.EventType = knowledgeevent.DraftUpdated
	workerSQLMust(t, inbox.AcceptSource(ctx, workerSQLDelivery(t, draft)))
	n, err = repo.SettleReady(ctx)
	workerSQLMust(t, err)
	if n != 1 {
		t.Fatalf("draft settle=%d", n)
	}
	var draftRow model.KnowledgeEventInbox
	workerSQLMust(t, db.Where("event_id = ?", draft.EventID).Take(&draftRow).Error)
	if draftRow.Status != model.KnowledgeEventIgnoredDraft {
		t.Fatal("draft indexed")
	}
}

func TestWorkerSnapshotProjectionMySQL(t *testing.T) {
	db, repo := workerSQLFixture(t)
	ctx := context.Background()
	source := workerSQLSource(t, db)
	versionID, _ := strconv.ParseUint(source.VersionID, 10, 64)
	article := model.RuleArticle{VersionID: versionID, ArticleOrder: 1, SectionPath: "test", ArticleNumber: "1", Content: "synthetic rule content"}
	workerSQLMust(t, db.Omit("Version").Create(&article).Error)
	claim, err := repo.ClaimLatest(ctx)
	workerSQLMust(t, err)
	if claim == nil {
		t.Fatal("no claim")
	}
	snapshot, err := repo.PrepareSnapshot(ctx, *claim)
	workerSQLMust(t, err)
	if len(snapshot.Prepared.Documents) != 1 {
		t.Fatalf("documents=%d", len(snapshot.Prepared.Documents))
	}
	var chunk model.KnowledgeChunk
	workerSQLMust(t, db.Where("article_id = ?", article.ID).Take(&chunk).Error)
	again, err := repo.PrepareSnapshot(ctx, *claim)
	workerSQLMust(t, err)
	if len(again.Prepared.Documents) != 1 {
		t.Fatal("snapshot changed")
	}
	var same model.KnowledgeChunk
	workerSQLMust(t, db.Where("article_id = ?", article.ID).Take(&same).Error)
	if same.ID != chunk.ID {
		t.Fatal("chunk identity changed")
	}
	workerSQLMust(t, db.Model(&chunk).Update("content", "corrupted projection").Error)
	if _, err := repo.PrepareSnapshot(ctx, *claim); !errors.Is(err, knowledgeworker.ErrProjectionConflict) {
		t.Fatalf("conflict=%v", err)
	}
	var retained model.RuleArticle
	workerSQLMust(t, db.First(&retained, article.ID).Error)
	if retained.Content != article.Content {
		t.Fatal("article modified")
	}
}

func TestWorkerReceiptCommitUnknownMySQL(t *testing.T) {
	db, repo := workerSQLFixture(t)
	ctx := context.Background()
	source := workerSQLSource(t, db)
	delivery := workerSQLDelivery(t, source)
	pool, err := db.DB()
	workerSQLMust(t, err)
	wrapped, err := gorm.Open(mysql.New(mysql.Config{Conn: workerCommitPool{pool}, SkipInitializeWithVersion: true}), &gorm.Config{DisableAutomaticPing: true, Logger: logger.Default.LogMode(logger.Silent)})
	workerSQLMust(t, err)
	uncertainInbox, err := NewKnowledgeInboxRepository(wrapped)
	workerSQLMust(t, err)
	if err := uncertainInbox.AcceptSource(ctx, delivery); !errors.Is(err, knowledgeworker.ErrSourceStore) {
		t.Fatalf("source unknown=%v", err)
	}
	inbox, err := NewKnowledgeInboxRepository(db)
	workerSQLMust(t, err)
	workerSQLMust(t, inbox.AcceptSource(ctx, delivery))
	workerSQLCount(t, db, &model.KnowledgeEventInbox{}, 1)
	claim, err := repo.ClaimLatest(ctx)
	workerSQLMust(t, err)
	if claim == nil {
		t.Fatal("missing claim")
	}
	workerSQLMust(t, repo.RecordBuildFailure(ctx, *claim, knowledgefailure.CodeDualExternalFailed))
	_, err = repo.RecoverPending(ctx)
	workerSQLMust(t, err)
	var row model.EventOutbox
	workerSQLMust(t, db.Take(&row).Error)
	d := eventbus.Delivery{Topic: row.Topic, Key: []byte(row.MessageKey), Value: row.Payload, Headers: []eventbus.Header{{Key: "message_id", Value: []byte(row.MessageID)}, {Key: "trace_id", Value: []byte(row.TraceID)}}}
	uncertain, err := NewKnowledgeBuildRepository(wrapped, 100)
	workerSQLMust(t, err)
	if err := uncertain.AcceptRetry(ctx, d); !errors.Is(err, knowledgeworker.ErrRetryStore) {
		t.Fatalf("retry unknown=%v", err)
	}
	workerSQLMust(t, repo.AcceptRetry(ctx, d))
	workerSQLCount(t, db, &model.KnowledgeRetryReceipt{}, 1)
	workerSQLCount(t, db, &model.KnowledgeIndexAttempt{}, 2)
	var scheduled model.KnowledgeIndexAttempt
	workerSQLMust(t, db.Where("attempt = 2").Take(&scheduled).Error)
	now, err := buildDatabaseTime(db)
	workerSQLMust(t, err)
	if !now.Before(scheduled.NotBefore) {
		t.Fatal("early scheduling probe missed its time window")
	}
	early, err := repo.ClaimLatest(ctx)
	workerSQLMust(t, err)
	if early != nil {
		t.Fatal("retry claimed before database due time")
	}
	d.Value = append(d.Value, ' ')
	if err := repo.AcceptRetry(ctx, d); !errors.Is(err, knowledgeworker.ErrRetryConflict) {
		t.Fatalf("altered retry=%v", err)
	}
}

func TestWorkerEpochBudgetMySQL(t *testing.T) {
	db, repo := workerSQLFixture(t)
	ctx := context.Background()
	source := workerSQLSource(t, db)
	inbox, err := NewKnowledgeInboxRepository(db)
	workerSQLMust(t, err)
	workerSQLMust(t, inbox.AcceptSource(ctx, workerSQLDelivery(t, source)))
	workerSQLMust(t, db.Model(&model.KnowledgeIndexTask{}).Where("source_epoch = 1").Update("failed_attempts", 4).Error)
	claim, err := repo.ClaimLatest(ctx)
	workerSQLMust(t, err)
	if claim == nil || claim.Attempt != 5 {
		t.Fatal("fifth task attempt missing")
	}
	workerSQLMust(t, repo.RecordBuildFailure(ctx, *claim, knowledgefailure.CodeDualExternalFailed))
	_, err = repo.RecoverPending(ctx)
	workerSQLMust(t, err)
	var event model.KnowledgeEventInbox
	workerSQLMust(t, db.Take(&event).Error)
	if event.Status != model.KnowledgeEventDeadLettered || event.ExpectedAttempt != 1 || event.SafeErrorCode != knowledgefailure.CodeEpochBudgetExhausted {
		t.Fatalf("event budget attribution: %+v", event)
	}
	workerSQLCount(t, db, &model.EventOutbox{}, 2)
	if _, err := repo.ClaimLatest(ctx); !errors.Is(err, knowledgeworker.ErrBuildExhausted) {
		t.Fatalf("exhausted=%v", err)
	}
}

func TestWorkerObservationSnapshotMySQL(t *testing.T) {
	db, repo := workerSQLFixture(t)
	ctx := context.Background()
	inserted := false
	workerSQLMust(t, db.Callback().Query().After("gorm:query").Register("test:between_snapshot_reads", func(tx *gorm.DB) {
		if inserted || tx.Statement.Schema == nil || tx.Statement.Schema.Table != "knowledge_index_controls" {
			return
		}
		inserted = true
		d := eventbus.Delivery{Topic: eventbus.TopicDocumentEvents, Offset: 400, Value: []byte("bad")}
		if err := repo.IsolatePoison(ctx, d, "source_record_invalid"); err != nil {
			tx.AddError(err)
		}
	}))
	snapshot, err := repo.Observe(ctx)
	workerSQLMust(t, db.Callback().Query().Remove("test:between_snapshot_reads"))
	workerSQLMust(t, err)
	if !inserted || snapshot.PoisonCount != 0 {
		t.Fatalf("RR leaked concurrent row: %d", snapshot.PoisonCount)
	}
	snapshot, err = repo.Observe(ctx)
	workerSQLMust(t, err)
	if snapshot.PoisonCount != 1 {
		t.Fatal("next snapshot did not see commit")
	}
	if snapshot.OldestDueSeconds != nil {
		t.Fatal("empty due set emitted number")
	}
	for _, b := range snapshot.Buckets {
		if b.Count == 0 && b.OldestAgeSeconds != nil {
			t.Fatal("empty state age must be null")
		}
	}
}

func TestWorkerControlLockCancellationMySQL(t *testing.T) {
	db, repo := workerSQLFixture(t)
	tx := db.Begin()
	workerSQLMust(t, tx.Error)
	defer tx.Rollback()
	workerSQLMust(t, tx.Exec("SELECT id FROM knowledge_index_controls WHERE id = 1 FOR UPDATE").Error)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	claim, err := repo.ClaimLatest(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || claim != nil {
		t.Fatalf("locked claim=%+v err=%v", claim, err)
	}
	workerSQLMust(t, tx.Rollback().Error)
	workerSQLCount(t, db, &model.KnowledgeIndexGeneration{}, 0)
	next, err := repo.ClaimLatest(context.Background())
	workerSQLMust(t, err)
	if next == nil {
		t.Fatal("lock not released")
	}
}

func TestWorkerLateRetryMySQL(t *testing.T) {
	db, repo := workerSQLFixture(t)
	ctx := context.Background()
	source := workerSQLSource(t, db)
	inbox, err := NewKnowledgeInboxRepository(db)
	workerSQLMust(t, err)
	workerSQLMust(t, inbox.AcceptSource(ctx, workerSQLDelivery(t, source)))
	claim, err := repo.ClaimLatest(ctx)
	workerSQLMust(t, err)
	if claim == nil {
		t.Fatal("no claim")
	}
	workerSQLMust(t, repo.RecordBuildFailure(ctx, *claim, knowledgefailure.CodeDualExternalFailed))
	_, err = repo.RecoverPending(ctx)
	workerSQLMust(t, err)
	var row model.EventOutbox
	workerSQLMust(t, db.Take(&row).Error)
	workerSQLMust(t, db.Model(&model.KnowledgeIndexControl{}).Where("id = 1").Update("desired_epoch", 2).Error)
	workerSQLMust(t, db.Model(&model.KnowledgeDocument{}).Where("id = ?", source.DocumentID).Update("publication_revision", 2).Error)
	d := eventbus.Delivery{Topic: row.Topic, Value: row.Payload, Key: []byte(row.MessageKey), Headers: []eventbus.Header{{Key: "message_id", Value: []byte(row.MessageID)}, {Key: "trace_id", Value: []byte(row.TraceID)}}}
	workerSQLMust(t, repo.AcceptRetry(ctx, d))
	var attempt model.KnowledgeIndexAttempt
	workerSQLMust(t, db.Where("attempt = 2").Take(&attempt).Error)
	if attempt.TargetEpoch == nil || *attempt.TargetEpoch != 2 {
		t.Fatal("old epoch reactivated")
	}
	if wait := time.Until(attempt.NotBefore) + 30*time.Millisecond; wait > 0 {
		if wait > 7*time.Second {
			t.Fatal("unexpected delay")
		}
		time.Sleep(wait)
	}
	n, err := repo.SettleReady(ctx)
	workerSQLMust(t, err)
	if n != 1 {
		t.Fatalf("late settlement=%d", n)
	}
	workerSQLMust(t, repo.AcceptRetry(ctx, d))
	workerSQLCount(t, db, &model.KnowledgeRetryReceipt{}, 1)
	workerSQLCount(t, db, &model.KnowledgeIndexAttempt{}, 2)
	var event model.KnowledgeEventInbox
	workerSQLMust(t, db.Take(&event).Error)
	if event.Status != model.KnowledgeEventSuperseded {
		t.Fatal("terminal resurrected")
	}
}

func TestWorkerLegacyRecoveryMySQL(t *testing.T) {
	db, repo := workerSQLFixture(t)
	ctx := context.Background()
	claim, err := repo.ClaimLatest(ctx)
	workerSQLMust(t, err)
	if claim == nil {
		t.Fatal("no claim")
	}
	workerSQLMust(t, repo.HoldFailure(ctx, *claim, knowledgefailure.CodeDualExternalFailed))
	adopted, err := repo.AdoptBlockedFailure(ctx)
	workerSQLMust(t, err)
	if !adopted {
		t.Fatal("legacy not adopted")
	}
	adopted, err = repo.AdoptBlockedFailure(ctx)
	workerSQLMust(t, err)
	if adopted {
		t.Fatal("legacy duplicated")
	}
	_, err = repo.RecoverPending(ctx)
	workerSQLMust(t, err)
	workerSQLCount(t, db, &model.KnowledgeBuildRecovery{}, 1)
	workerSQLCount(t, db, &model.EventOutbox{}, 0)
	var task model.KnowledgeIndexTask
	workerSQLMust(t, db.Take(&task).Error)
	if task.FailedAttempts != 1 || task.Status != model.KnowledgeTaskRetryWait {
		t.Fatal("legacy count/state changed")
	}
}

func TestWorkerEmptyPublicationMySQL(t *testing.T) {
	db, repo := workerSQLFixture(t)
	ctx := context.Background()
	claim, err := repo.ClaimLatest(ctx)
	workerSQLMust(t, err)
	if claim == nil {
		t.Fatal("no claim")
	}
	snapshot, err := repo.PrepareSnapshot(ctx, *claim)
	workerSQLMust(t, err)
	if len(snapshot.Prepared.Documents) != 0 {
		t.Fatal("nonempty fixture")
	}
	identity := embedding.ModelIdentity{Model: embedding.ModelName, Digest: "7907646426070047a77226ac3e684fbbe8410524f7b4a74d02837e43f2146bab", Dimensions: embedding.Dimensions, ContextLength: embedding.ContextLength, InputPolicy: embedding.InputPolicy, ServerVersion: "test-identity"}
	workerSQLMust(t, repo.StartGeneration(ctx, *claim, snapshot, identity))
	workerSQLMust(t, repo.AdvanceGeneration(ctx, *claim, snapshot.ManifestSHA256, knowledgeworker.PhasePrepared, knowledgeworker.PhaseSealed))
	verified := &knowledgeworker.VerifiedGeneration{Claim: *claim, Snapshot: snapshot, Identity: identity}
	pool, err := db.DB()
	workerSQLMust(t, err)
	wrapped, err := gorm.Open(mysql.New(mysql.Config{Conn: workerCommitPool{pool}, SkipInitializeWithVersion: true}), &gorm.Config{DisableAutomaticPing: true, Logger: logger.Default.LogMode(logger.Silent)})
	workerSQLMust(t, err)
	uncertain, err := NewKnowledgeBuildRepository(wrapped, 100)
	workerSQLMust(t, err)
	if err := uncertain.Publish(ctx, verified); !errors.Is(err, knowledgeworker.ErrBuildStore) {
		t.Fatalf("unknown publish=%v", err)
	}
	workerSQLMust(t, repo.Publish(ctx, verified))
	resolver, err := NewKnowledgeActiveRepository(db)
	workerSQLMust(t, err)
	target, err := resolver.ResolveActive(ctx)
	workerSQLMust(t, err)
	if !target.Empty || target.GenerationID != claim.GenerationID || target.Epoch != 1 {
		t.Fatalf("active=%+v", target)
	}
	workerSQLMust(t, db.Model(&model.KnowledgeIndexControl{}).Where("id = 1").Update("desired_epoch", 2).Error)
	next, err := repo.ClaimLatest(ctx)
	workerSQLMust(t, err)
	if next == nil {
		t.Fatal("no next claim")
	}
	nextSnapshot, err := repo.PrepareSnapshot(ctx, *next)
	workerSQLMust(t, err)
	workerSQLMust(t, repo.StartGeneration(ctx, *next, nextSnapshot, identity))
	workerSQLMust(t, repo.AdvanceGeneration(ctx, *next, nextSnapshot.ManifestSHA256, knowledgeworker.PhasePrepared, knowledgeworker.PhaseSealed))
	workerSQLMust(t, db.Model(&model.KnowledgeIndexControl{}).Where("id = 1").Update("desired_epoch", 3).Error)
	if err := repo.Publish(ctx, &knowledgeworker.VerifiedGeneration{Claim: *next, Snapshot: nextSnapshot, Identity: identity}); !errors.Is(err, knowledgeworker.ErrBuildObsolete) {
		t.Fatalf("old publish=%v", err)
	}
	target, err = resolver.ResolveActive(ctx)
	workerSQLMust(t, err)
	if target.GenerationID != claim.GenerationID {
		t.Fatal("obsolete overwrote active")
	}
}
