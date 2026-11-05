package router

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
	"video-support-agent/internal/auth"
	"video-support-agent/internal/config"
	"video-support-agent/internal/database"
	"video-support-agent/internal/eventbus"
	"video-support-agent/internal/knowledgeevent"
	"video-support-agent/internal/model"
	"video-support-agent/internal/repository"
	"video-support-agent/internal/service"
)

func publishingRows(t *testing.T, db *gorm.DB) []model.EventOutbox {
	t.Helper()
	var rows []model.EventOutbox
	Must(t, db.Order("id").Find(&rows).Error)
	return rows
}
func publishingControl(t *testing.T, db *gorm.DB) model.KnowledgeIndexControl {
	t.Helper()
	var c model.KnowledgeIndexControl
	Must(t, db.First(&c, 1).Error)
	return c
}

func TestPublishingAtomicEventsMySQL(t *testing.T) {
	for _, kind := range []string{"create", "edit_unverified", "edit_verified", "revision", "publish", "withdraw"} {
		t.Run(kind, func(t *testing.T) {
			f := newPublicationFixture(t)
			at := publicationTime().Add(-time.Hour)
			v := f.draft(t, &at, nil, kind == "publish" || kind == "withdraw" || kind == "edit_verified")
			if kind == "withdraw" {
				_, err := f.svc(f.observer).PublishVersion(f.ctx, f.userID, publicationInput(v))
				Must(t, err)
			}
			trace := "1234567890abcdef1234567890abcdef"
			ctx, err := knowledgeevent.WithTrace(f.ctx, trace)
			Must(t, err)
			key := f.prefix + "_new"
			f.keys = append(f.keys, key)
			action := func() error {
				s := f.svc(f.db)
				switch kind {
				case "create":
					input := Input()
					input.DocumentKey = key
					input.CategoryID = f.category
					_, e := s.CreateDraft(ctx, input)
					return e
				case "edit_unverified", "edit_verified":
					_, e := s.EditDraftWithVerification(ctx, publicationEdit(v))
					return e
				case "revision":
					_, e := s.CreateRevision(ctx, service.CreateKnowledgeRevisionInput{DocumentID: v.DocumentID, Version: "revision", RevisionType: model.RuleRevisionTypeEditorial, RevisionNote: "虚构测试修订", CategoryID: f.category, Title: v.Title, Content: v.Content, SourceURL: v.SourceURL, EffectiveAt: &at, Articles: []service.CreateRuleArticleInput{{Content: "虚构条款"}}})
					return e
				case "publish":
					_, e := s.PublishVersion(ctx, f.userID, publicationInput(v))
					return e
				default:
					_, e := s.WithdrawVersion(ctx, f.userID, service.WithdrawKnowledgeVersionInput{DocumentID: v.DocumentID, VersionID: v.ID, ExpectedPublicationRevision: 1})
					return e
				}
			}
			before := f.state(t, v.ID)
			counts := Counts(t, f.observer)
			outbox := publishingRows(t, f.observer)
			control := publishingControl(t, f.observer)
			injected := false
			fault := errors.New("test outbox write failure")
			callback := f.prefix + "_outbox"
			Must(t, f.db.Callback().Create().After("gorm:create").Register(callback, func(tx *gorm.DB) {
				if tx.Error == nil && tx.Statement.Schema != nil && tx.Statement.Schema.Table == "event_outboxes" {
					injected = true
					tx.AddError(fault)
				}
			}))
			err = action()
			Must(t, f.db.Callback().Create().Remove(callback))
			if !injected || !errors.Is(err, fault) {
				t.Fatalf("fault not reached: %v", err)
			}
			Equal(t, f.state(t, v.ID), before)
			Equal(t, Counts(t, f.observer), counts)
			Equal(t, publishingRows(t, f.observer), outbox)
			Equal(t, publishingControl(t, f.observer), control)
			Must(t, action())
			after := publishingRows(t, f.observer)
			if len(after) != len(outbox)+1 {
				t.Fatal("not exactly one event")
			}
			row := after[len(after)-1]
			source, e := knowledgeevent.Decode(row.Payload, row.MessageKey)
			Must(t, e)
			want := map[string]string{"create": knowledgeevent.DraftCreated, "edit_unverified": knowledgeevent.DraftUpdated, "edit_verified": knowledgeevent.DraftUpdated, "revision": knowledgeevent.RevisionCreated, "publish": knowledgeevent.VersionPublished, "withdraw": knowledgeevent.VersionWithdrawn}[kind]
			sum := sha256.Sum256(row.Payload)
			if source.EventType != want || source.TraceID != trace || row.TraceID != trace || source.EventID != row.EventID || row.MessageID == row.EventID || row.DedupKey != "source:"+row.EventID || row.PayloadSHA256 != hex.EncodeToString(sum[:]) || row.Status != "pending" || row.Topic != eventbus.TopicDocumentEvents {
				t.Fatalf("bad source event: %+v", source)
			}
			expected := control.DesiredEpoch
			if kind == "publish" || kind == "withdraw" {
				expected++
			}
			Equal(t, publishingControl(t, f.observer).DesiredEpoch, expected)
			t.Log("real INSERT fault rolled back content, operation, epoch and event; success committed one", want)
		})
	}
}

func TestPublishingLeaseMySQL(t *testing.T) {
	f := newPublicationFixture(t)
	// This suite owns the entire ephemeral database; remove only test outbox rows.
	Must(t, f.observer.Exec("DELETE FROM event_outboxes").Error)
	seed := func() model.EventOutbox {
		id, e := knowledgeevent.NewID()
		Must(t, e)
		row := model.EventOutbox{MessageID: id, EventID: id, DedupKey: "test:" + id, Topic: eventbus.TopicDocumentEvents, MessageKey: "1", TraceID: "1234567890abcdef1234567890abcdef", Payload: []byte(`{"test":true}`), PayloadSHA256: strings.Repeat("0", 64), Status: model.OutboxPending, NextAttemptAt: time.Now().UTC().Add(-time.Hour)}
		Must(t, f.observer.Create(&row).Error)
		return row
	}
	r1, e := repository.NewOutboxRepository(f.db)
	Must(t, e)
	r2, e := repository.NewOutboxRepository(f.second)
	Must(t, e)
	first := seed()
	second := seed()
	var claims [2]*model.EventOutbox
	var errs [2]error
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i, r := range []*repository.OutboxRepository{r1, r2} {
		wg.Add(1)
		go func() { defer wg.Done(); <-start; claims[i], errs[i] = r.ClaimOne(f.ctx) }()
	}
	close(start)
	wg.Wait()
	for i := range claims {
		Must(t, errs[i])
		if claims[i] == nil || claims[i].Attempts != 1 || claims[i].OwnerToken == nil {
			t.Fatal("missing claim")
		}
	}
	if claims[0].ID == claims[1].ID {
		t.Fatal("duplicate live claim")
	}
	t.Logf("concurrent owners claimed distinct rows %d and %d", claims[0].ID, claims[1].ID)
	c := claims[0]
	Must(t, f.observer.Model(&model.EventOutbox{}).Where("id = ?", c.ID).Update("lease_until", gorm.Expr("UTC_TIMESTAMP(3) - INTERVAL 1 SECOND")).Error)
	taken, e := r2.ClaimOne(f.ctx)
	Must(t, e)
	if taken == nil || taken.ID != c.ID || taken.Attempts != 2 || *taken.OwnerToken == *c.OwnerToken {
		t.Fatal("bad takeover")
	}
	if e := r1.MarkSent(f.ctx, c.MessageID, *c.OwnerToken); !errors.Is(e, repository.ErrOutboxLeaseLost) {
		t.Fatalf("old owner accepted: %v", e)
	}
	Must(t, r2.MarkRetry(f.ctx, taken.MessageID, *taken.OwnerToken, repository.OutboxDeliveryUnknown, 1500*time.Millisecond))
	var delay int64
	Must(t, f.observer.Raw("SELECT TIMESTAMPDIFF(MICROSECOND,updated_at,next_attempt_at) FROM event_outboxes WHERE id=?", taken.ID).Scan(&delay).Error)
	Equal(t, delay, int64(1500000))
	Must(t, r1.MarkSent(f.ctx, claims[1].MessageID, *claims[1].OwnerToken))
	if e := r1.MarkSent(f.ctx, claims[1].MessageID, *claims[1].OwnerToken); !errors.Is(e, repository.ErrOutboxLeaseLost) {
		t.Fatal("second settlement allowed")
	}
	Must(t, f.observer.Model(&model.EventOutbox{}).Where("id IN ?", []uint64{first.ID, second.ID}).Update("status", model.OutboxSent).Error)
	overflow := seed()
	Must(t, f.observer.Model(&model.EventOutbox{}).Where("id=?", overflow.ID).Update("attempts", ^uint64(0)).Error)
	got, e := r1.ClaimOne(f.ctx)
	Must(t, e)
	if got != nil {
		t.Fatal("overflow returned sendable row")
	}
	Must(t, f.observer.First(&overflow, overflow.ID).Error)
	Equal(t, overflow.Status, model.OutboxBlocked)
	Equal(t, overflow.SafeErrorCode, repository.OutboxAttemptsOverflow)
	t.Run("skip_locked", func(t *testing.T) {
		a := seed()
		b := seed()
		tx := f.db.Begin()
		Must(t, tx.Error)
		defer tx.Rollback()
		Must(t, tx.Exec("SELECT id FROM event_outboxes WHERE id=? FOR UPDATE", a.ID).Error)
		got, e := r2.ClaimOne(f.ctx)
		Must(t, e)
		if got == nil || got.ID != b.ID {
			t.Fatal("locked row not skipped")
		}
		Must(t, tx.Rollback().Error)
		Must(t, f.observer.Model(&model.EventOutbox{}).Where("id IN ?", []uint64{a.ID, b.ID}).Update("status", model.OutboxSent).Error)
	})
	t.Run("expiry_during_lock_wait", func(t *testing.T) {
		a := seed()
		claimed, e := r1.ClaimOne(f.ctx)
		Must(t, e)
		if claimed == nil || claimed.ID != a.ID {
			t.Fatal("claim wrong row")
		}
		Must(t, f.observer.Model(&model.EventOutbox{}).Where("id=?", a.ID).Update("lease_until", gorm.Expr("UTC_TIMESTAMP(3) + INTERVAL 2 SECOND")).Error)
		tx := f.db.Begin()
		Must(t, tx.Error)
		defer tx.Rollback()
		Must(t, tx.Exec("SELECT id FROM event_outboxes WHERE id=? FOR UPDATE", a.ID).Error)
		done := make(chan error, 1)
		go func() { done <- r2.MarkSent(f.ctx, a.MessageID, *claimed.OwnerToken) }()
		deadline := time.Now().Add(4 * time.Second)
		observed := false
		for time.Now().Before(deadline) {
			var count int64
			Must(t, f.observer.Raw("SELECT COUNT(*) FROM performance_schema.data_lock_waits").Scan(&count).Error)
			if count > 0 {
				observed = true
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if !observed {
			t.Fatal("no actual InnoDB lock wait observed")
		}
		t.Log("performance_schema.data_lock_waits confirms blocked settlement")
		for {
			var expired bool
			Must(t, f.observer.Raw("SELECT lease_until <= UTC_TIMESTAMP(3) FROM event_outboxes WHERE id=?", a.ID).Scan(&expired).Error)
			if expired {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("database lease did not expire")
			}
			time.Sleep(10 * time.Millisecond)
		}
		select {
		case e := <-done:
			t.Fatalf("settled while row lock held: %v", e)
		default:
		}
		Must(t, tx.Rollback().Error)
		select {
		case e := <-done:
			if !errors.Is(e, repository.ErrOutboxLeaseLost) {
				t.Fatalf("expired lease accepted %v", e)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("settlement stuck")
		}
	})
}

func TestPublishingDeliveryRouterMySQL(t *testing.T) {
	f := newPublicationFixture(t)
	v := f.draft(t, nil, nil, false)
	_, err := f.svc(f.observer).EditDraftWithVerification(f.ctx, publicationEdit(v))
	Must(t, err)
	other := f.draft(t, nil, nil, false)
	tokens, err := auth.NewTokenManager(strings.Repeat("p", 32), "publishing-test", time.Hour)
	Must(t, err)
	admin, err := tokens.Issue(f.userID)
	Must(t, err)
	r := New(config.Config{AppName: "publishing-test"}, f.db, tokens, nil, AllowLimiter{}, nil, zap.NewNop(), nil)
	path := fmt.Sprintf("/api/v1/admin/knowledge/documents/%d/deliveries", v.DocumentID)
	for _, role := range []string{"", model.RoleUser, model.RoleSupport} {
		token := ""
		want := 401
		if role != "" {
			u := model.User{Username: f.prefix + role, PasswordHash: "test-only", Role: role}
			Must(t, f.observer.Create(&u).Error)
			t.Cleanup(func() { Must(t, f.observer.Delete(&u).Error) })
			token, err = tokens.Issue(u.ID)
			Must(t, err)
			want = 403
		}
		w := Request(r, "GET", path, "", token, false)
		Equal(t, w.Code, want)
	}
	w := Request(r, "GET", path+"?limit=1", "", admin, false)
	Equal(t, w.Code, 200)
	var page service.KnowledgeDeliveryPage
	Must(t, json.Unmarshal(w.Body.Bytes(), &page))
	if len(page.Items) != 1 || page.NextBefore == "" || page.DocumentID != v.DocumentID {
		t.Fatal("bad page")
	}
	if w.Header().Get("X-Trace-ID") == "" {
		t.Fatal("no request trace")
	}
	for _, forbidden := range []string{"payload", "owner_token", "lease_until", "dedup_key"} {
		if strings.Contains(w.Body.String(), forbidden) {
			t.Fatal("leaked internal field")
		}
	}
	w = Request(r, "GET", path+"?limit=1&before="+page.NextBefore, "", admin, false)
	Equal(t, w.Code, 200)
	var next service.KnowledgeDeliveryPage
	Must(t, json.Unmarshal(w.Body.Bytes(), &next))
	if len(next.Items) != 1 || next.Items[0].ID >= page.Items[0].ID || next.NextBefore != "" {
		t.Fatal("bad next page")
	}
	rows, e := repository.NewKnowledgeRepository(f.observer).ReadDocumentDeliveries(f.ctx, other.DocumentID, 0, 100)
	Must(t, e)
	if len(rows) != 1 {
		t.Fatal("cross-document leak")
	}
	w = Request(r, "GET", "/api/v1/admin/knowledge/documents/18446744073709551615/deliveries", "", admin, false)
	Equal(t, w.Code, 404)
	var cols []string
	Must(t, f.observer.Raw("SELECT COLUMN_NAME FROM information_schema.STATISTICS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='event_outboxes' AND INDEX_NAME='idx_outbox_document' ORDER BY SEQ_IN_INDEX").Scan(&cols).Error)
	Equal(t, strings.Join(cols, ","), "topic,message_key,id")
	// Derived topic rows share a document key but must not be listed.
	id, e := knowledgeevent.NewID()
	Must(t, e)
	derived := model.EventOutbox{MessageID: id, EventID: id, DedupKey: "derived:" + id, Topic: eventbus.TopicIndexRetry, MessageKey: strconv.FormatUint(other.DocumentID, 10), Payload: []byte("{}"), PayloadSHA256: "test", TraceID: "test", Status: "pending", NextAttemptAt: time.Now()}
	Must(t, f.observer.Create(&derived).Error)
	rows, e = repository.NewKnowledgeRepository(f.observer).ReadDocumentDeliveries(f.ctx, other.DocumentID, 0, 100)
	Must(t, e)
	Equal(t, len(rows), 1)
}

func TestPublishingInitializationMySQL(t *testing.T) {
	f := newPublicationFixture(t)
	before := publishingControl(t, f.observer)
	Must(t, database.Migrate(f.db))
	Equal(t, publishingControl(t, f.observer), before)
	var n int64
	Must(t, f.observer.Model(&model.KnowledgeIndexBootstrap{}).Count(&n).Error)
	Equal(t, n, int64(1))
	Must(t, f.observer.Model(&model.KnowledgeIndexControl{}).Where("id=1").Update("desired_epoch", before.DesiredEpoch+10).Error)
	Must(t, database.Migrate(f.db))
	Equal(t, publishingControl(t, f.observer).DesiredEpoch, before.DesiredEpoch+10)
	t.Log("repeated migration preserves singleton and advanced epoch")
}

// Execute a real COMMIT, then hide its successful result from the repository.
// This is controlled client-boundary fault injection, not a TCP outage simulation.
type publishingCommitPool struct{ *sql.DB }

func (p publishingCommitPool) BeginTx(ctx context.Context, opts *sql.TxOptions) (gorm.ConnPool, error) {
	tx, e := p.DB.BeginTx(ctx, opts)
	if e != nil {
		return nil, e
	}
	return &publishingCommitTx{tx}, nil
}

type publishingCommitTx struct{ *sql.Tx }

func (tx publishingCommitTx) Commit() error {
	if e := tx.Tx.Commit(); e != nil {
		return e
	}
	return errors.New("injected commit acknowledgement loss")
}

func TestPublishingCommitUnknownMySQL(t *testing.T) {
	f := newPublicationFixture(t)
	Must(t, f.observer.Exec("DELETE FROM event_outboxes").Error)
	v := f.draft(t, nil, nil, false)
	pool, e := f.db.DB()
	Must(t, e)
	wrapped, e := gorm.Open(gormmysql.New(gormmysql.Config{Conn: publishingCommitPool{pool}, SkipInitializeWithVersion: true}), &gorm.Config{DisableAutomaticPing: true, Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
	Must(t, e)
	r, e := repository.NewOutboxRepository(wrapped)
	Must(t, e)
	row, e := r.ClaimOne(f.ctx)
	if e == nil || row != nil {
		t.Fatalf("unknown commit returned sendable row: %v %+v", e, row)
	}
	var saved model.EventOutbox
	Must(t, f.observer.Where("message_key=?", strconv.FormatUint(v.DocumentID, 10)).Take(&saved).Error)
	if saved.Status != model.OutboxSending || saved.Attempts != 1 || saved.OwnerToken == nil {
		t.Fatal("real commit not observable")
	}
	t.Log("independent MySQL observer sees committed claim; caller receives error and nil row")
	// MarkSent has the same unknown-commit boundary, without undoing the commit.
	if e := r.MarkSent(f.ctx, saved.MessageID, *saved.OwnerToken); e == nil {
		t.Fatal("missing injected error")
	}
	Must(t, f.observer.First(&saved, saved.ID).Error)
	Equal(t, saved.Status, model.OutboxSent)
}

func TestPublishingBootstrapRollbackAndUniquenessMySQL(t *testing.T) {
	f := newPublicationFixture(t)
	original := publishingControl(t, f.observer)
	var bootstrap model.KnowledgeIndexBootstrap
	Must(t, f.observer.First(&bootstrap, 1).Error)
	Must(t, f.observer.Exec("DELETE FROM knowledge_index_bootstraps").Error)
	Must(t, f.observer.Exec("DELETE FROM knowledge_index_controls").Error)
	callback := f.prefix + "_bootstrap"
	injected := false
	fault := errors.New("bootstrap fault")
	Must(t, f.db.Callback().Create().After("gorm:create").Register(callback, func(tx *gorm.DB) {
		if tx.Error == nil && tx.Statement.Schema != nil && tx.Statement.Schema.Table == "knowledge_index_bootstraps" {
			injected = true
			tx.AddError(fault)
		}
	}))
	e := database.Migrate(f.db)
	Must(t, f.db.Callback().Create().Remove(callback))
	if !injected || !errors.Is(e, fault) {
		t.Fatalf("bootstrap injection missing %v", e)
	}
	for _, table := range []string{"knowledge_index_bootstraps", "knowledge_index_controls"} {
		var n int64
		Must(t, f.observer.Table(table).Count(&n).Error)
		Equal(t, n, int64(0))
	}
	Must(t, database.Migrate(f.db))
	fresh := publishingControl(t, f.observer)
	Equal(t, fresh.DesiredEpoch, uint64(1))
	Equal(t, fresh.ActiveEpoch, uint64(0))
	Must(t, f.observer.Model(&model.KnowledgeIndexControl{}).Where("id=1").Update("desired_epoch", original.DesiredEpoch).Error)
	v := f.draft(t, nil, nil, false)
	var row model.EventOutbox
	Must(t, f.observer.Where("message_key=?", strconv.FormatUint(v.DocumentID, 10)).Take(&row).Error)
	for _, which := range []string{"message", "dedup"} {
		duplicate := row
		duplicate.ID = 0
		id, e := knowledgeevent.NewID()
		Must(t, e)
		if which == "message" {
			duplicate.DedupKey = "unique:" + id
		} else {
			duplicate.MessageID = id
		}
		if e := f.observer.Create(&duplicate).Error; e == nil {
			t.Fatalf("%s uniqueness missing", which)
		}
	}
	duplicate := row
	duplicate.ID = 0
	id, e := knowledgeevent.NewID()
	Must(t, e)
	duplicate.MessageID = id
	duplicate.DedupKey = "result:" + id
	Must(t, f.observer.Create(&duplicate).Error)
	t.Log("bootstrap/control rollback together; message and purpose unique, event_id may repeat")
}

func TestPublishingEpochSerializationMySQL(t *testing.T) {
	f := newPublicationFixture(t)
	at := publicationTime().Add(-time.Hour)
	a := f.draft(t, &at, nil, true)
	b := f.draft(t, &at, nil, true)
	before := publishingControl(t, f.observer).DesiredEpoch
	start := make(chan struct{})
	done := make(chan error, 2)
	for i, v := range []*model.RuleVersion{a, b} {
		db := []*gorm.DB{f.db, f.second}[i]
		go func() { <-start; _, e := f.svc(db).PublishVersion(f.ctx, f.userID, publicationInput(v)); done <- e }()
	}
	close(start)
	Must(t, <-done)
	Must(t, <-done)
	Equal(t, publishingControl(t, f.observer).DesiredEpoch, before+2)
	// Failed optimistic publication must not consume a global epoch or emit an event.
	rows := len(publishingRows(t, f.observer))
	_, e := f.svc(f.db).PublishVersion(f.ctx, f.userID, publicationInput(a))
	if e == nil {
		t.Fatal("stale publish accepted")
	}
	Equal(t, publishingControl(t, f.observer).DesiredEpoch, before+2)
	Equal(t, len(publishingRows(t, f.observer)), rows)
}
