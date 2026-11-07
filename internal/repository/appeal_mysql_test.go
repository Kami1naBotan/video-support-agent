//go:build mysql

package repository

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"video-support-agent/internal/model"
	"video-support-agent/internal/service"

	driver "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func SQLNumber(t *testing.T, err error, number uint16) {
	t.Helper()
	var mysqlErr *driver.MySQLError
	if !errors.As(err, &mysqlErr) || mysqlErr.Number != number {
		t.Fatalf("expected MySQL %d, got %v", number, err)
	}
}

func SQLSchema(t *testing.T, db *gorm.DB) {
	t.Helper()
	for _, tc := range []struct{ table, index, columns string }{
		{"appeal_tickets", "uk_appeal_ticket_user_key", "user_id,idempotency_key"},
		{"ticket_records", "uk_ticket_record_sequence", "ticket_id,sequence"},
	} {
		var columns, engine string
		SQLMust(t, db.Raw(
			"SELECT COALESCE(GROUP_CONCAT(column_name ORDER BY seq_in_index SEPARATOR ','),'') FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name=? AND index_name=? AND non_unique=0",
			tc.table, tc.index,
		).Scan(&columns).Error)
		SQLMust(t, db.Raw(
			"SELECT engine FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name=?",
			tc.table,
		).Scan(&engine).Error)
		if columns != tc.columns || engine != "InnoDB" {
			t.Fatalf("schema %s index=%s engine=%s", tc.table, columns, engine)
		}
		t.Logf("MYSQL_INDEX table=%s name=%s columns=%s engine=%s",
			tc.table, tc.index, columns, engine)
	}
	for _, tc := range []struct{ column, typ string }{
		{"eligibility_snapshot", "json"},
		{"created_at", "datetime"},
		{"updated_at", "datetime"},
	} {
		var typ string
		SQLMust(t, db.Raw(
			"SELECT data_type FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name='appeal_tickets' AND column_name=?",
			tc.column,
		).Scan(&typ).Error)
		if typ != tc.typ {
			t.Fatalf("column=%s type=%s", tc.column, typ)
		}
	}
	var precision int
	SQLMust(t, db.Raw(
		"SELECT datetime_precision FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name='appeal_tickets' AND column_name='created_at'",
	).Scan(&precision).Error)
	if precision != 3 {
		t.Fatalf("datetime precision=%d", precision)
	}
	for _, column := range []string{"idempotency_key", "request_hash"} {
		var collation string
		SQLMust(t, db.Raw(
			"SELECT collation_name FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name='appeal_tickets' AND column_name=?",
			column,
		).Scan(&collation).Error)
		if collation != "ascii_bin" {
			t.Fatalf("column=%s collation=%s", column, collation)
		}
	}
}

func TestMySQL(t *testing.T) {
	if !t.Run("fresh_migration", func(t *testing.T) {
		db, _ := SQLDatabase(t)
		SQLMigrate(t, db)
		SQLMigrate(t, db)
		SQLSchema(t, db)
	}) {
		t.Fatal("fresh migration prerequisite failed")
	}

	db, cfg := SQLDatabase(t)
	SQLMust(t, db.AutoMigrate(SQLLegacyModels()...))
	f := SQLSeed(t, db, cfg)

	if !t.Run("upgrade_preserves_data", func(t *testing.T) {
		if db.Migrator().HasTable(&model.AppealTicket{}) {
			t.Fatal("legacy setup already contains ticket table")
		}
		SQLMigrate(t, db)
		SQLMigrate(t, db)

		var user model.User
		var conversation model.Conversation
		SQLMust(t, db.First(&user, f.user.ID).Error)
		SQLMust(t, db.First(&conversation, f.conversation.ID).Error)
		if user.Username != f.user.Username ||
			conversation.UserID != user.ID ||
			conversation.Title != f.conversation.Title {
			t.Fatal("legacy data changed")
		}
		SQLSchema(t, db)
	}) {
		t.Fatal("upgrade prerequisite failed")
	}

	r := SQLRepo(t, db)

	t.Run("roundtrip_constraints", func(t *testing.T) {
		ctx := SQLContext(t)
		c := f.command(t)
		result, err := r.CreateWithRecord(ctx, c, SQLCheck)
		SQLMust(t, err)
		if result == nil || result.Replayed ||
			result.InitialStatus != model.AppealTicketPendingReview {
			t.Fatalf("result=%+v", result)
		}
		ticket, err := r.GetByID(ctx, c.UserID, result.TicketID)
		SQLMust(t, err)
		expected, err := SQLCheck(ctx, nil, SQLAt)
		SQLMust(t, err)
		if !reflect.DeepEqual(ticket.EligibilitySnapshot, expected) ||
			ticket.Reason != c.Input.Reason ||
			ticket.SubjectRef != c.Input.SubjectRef ||
			!ticket.CreatedAt.Equal(SQLAt) ||
			!ticket.UpdatedAt.Equal(SQLAt) {
			t.Fatalf("roundtrip=%+v", ticket)
		}
		var record model.TicketRecord
		SQLMust(t, db.Where("ticket_id = ?", ticket.ID).Take(&record).Error)
		if record.Sequence != 1 || record.FromStatus != nil ||
			record.Action != model.TicketRecordCreated ||
			record.ActorUserID != c.UserID || record.ToStatus != ticket.Status ||
			record.ReasonCode != "manual_review_requested" ||
			!record.CreatedAt.Equal(ticket.CreatedAt) {
			t.Fatalf("record=%+v", record)
		}

		repeated := *ticket
		repeated.ID = 0
		SQLNumber(t,
			db.Omit(clause.Associations).Create(&repeated).Error, 1062)
		record.ID = 0
		SQLNumber(t,
			db.Omit(clause.Associations).Create(&record).Error, 1062)

		for _, kind := range []string{"user", "conversation"} {
			bad := *ticket
			bad.ID = 0
			bad.IdempotencyKey = SQLKey()
			if kind == "user" {
				bad.UserID = 999999999
			} else {
				bad.ConversationID = 999999999
			}
			SQLNumber(t,
				db.Omit(clause.Associations).Create(&bad).Error, 1452)
		}
		for _, kind := range []string{"ticket", "actor"} {
			bad := record
			bad.ID = 0
			bad.Sequence = 2
			if kind == "ticket" {
				bad.TicketID = 999999999
			} else {
				bad.ActorUserID = 999999999
			}
			SQLNumber(t,
				db.Omit(clause.Associations).Create(&bad).Error, 1452)
		}
		SQLNumber(t, db.Delete(&model.AppealTicket{}, ticket.ID).Error, 1451)
		SQLCounts(t, db, c, 1, 1)
	})

	t.Run("idempotency_ownership", func(t *testing.T) {
		ctx := SQLContext(t)
		c := f.command(t)
		first, err := r.CreateWithRecord(ctx, c, SQLCheck)
		SQLMust(t, err)

		replayCommand := c
		replayCommand.ExpectedPolicyDigest = ""
		for i := 0; i < 3; i++ {
			again, err := r.CreateWithRecord(ctx, replayCommand, nil)
			SQLMust(t, err)
			if again.TicketID != first.TicketID || !again.Replayed ||
				!again.CreatedAt.Equal(first.CreatedAt) {
				t.Fatal("unstable replay")
			}
		}

		changed := c
		changed.Input.Reason += "changed"
		changed.RequestHash, err = service.AppealRequestHash(
			changed.ConversationID, changed.Input,
		)
		SQLMust(t, err)
		if _, err = r.CreateWithRecord(ctx, changed, SQLCheck); !errors.Is(err, service.ErrAppealIdempotencyConflict) {
			t.Fatalf("conflict=%v", err)
		}
		if _, err = r.GetByID(ctx, f.other.ID, first.TicketID); !errors.Is(err, service.ErrAppealTicketNotFound) {
			t.Fatalf("foreign ticket=%v", err)
		}
		if _, err = r.GetByOperation(ctx, f.other.ID, c.IdempotencyKey); !errors.Is(err, service.ErrAppealTicketNotFound) {
			t.Fatalf("foreign operation=%v", err)
		}

		wrong := f.command(t)
		wrong.ConversationID = f.otherConversation.ID
		wrong.RequestHash, err = service.AppealRequestHash(
			wrong.ConversationID, wrong.Input,
		)
		SQLMust(t, err)
		if _, err = r.CreateWithRecord(ctx, wrong, SQLCheck); !errors.Is(err, service.ErrConversationNotFound) {
			t.Fatalf("foreign conversation=%v", err)
		}
		SQLCounts(t, db, wrong, 0, 0)

		another := c
		another.IdempotencyKey = SQLKey()
		newResult, err := r.CreateWithRecord(ctx, another, SQLCheck)
		SQLMust(t, err)
		if newResult.TicketID == first.TicketID {
			t.Fatal("new key incorrectly deduplicated")
		}

		other := c
		other.UserID, other.ConversationID = f.other.ID, f.otherConversation.ID
		other.RequestHash, err = service.AppealRequestHash(
			other.ConversationID, other.Input,
		)
		SQLMust(t, err)
		otherResult, err := r.CreateWithRecord(ctx, other, SQLCheck)
		SQLMust(t, err)
		if otherResult.TicketID == first.TicketID {
			t.Fatal("cross-user deduplication")
		}
		for _, command := range []service.AppealCreateCommand{c, another, other} {
			SQLCounts(t, db, command, 1, 1)
		}
	})

	t.Run("second_insert_rolls_back", func(t *testing.T) {
		failingDB := SQLOpen(t, cfg)
		var injected atomic.Bool
		SQLMust(t, failingDB.Callback().Create().
			Before("gorm:create").
			Register(":bad_record_fk", func(tx *gorm.DB) {
				if tx.Statement.Schema != nil &&
					tx.Statement.Schema.Table == "ticket_records" {
					injected.Store(true)
					tx.Statement.SetColumn("TicketID", uint64(999999999))
				}
			}))

		c := f.command(t)
		result, err := SQLRepo(t, failingDB).
			CreateWithRecord(SQLContext(t), c, SQLCheck)
		if result != nil || !injected.Load() {
			t.Fatal("second insert failure not reached")
		}
		SQLNumber(t, err, 1452)
		SQLCounts(t, db, c, 0, 0)

		// 失败后同一个原键重新满足条件再创建。
		_, err = r.CreateWithRecord(SQLContext(t), c, SQLCheck)
		SQLMust(t, err)
		SQLCounts(t, db, c, 1, 1)
	})

	for _, different := range []bool{false, true} {
		name := "concurrent_same_payload"
		if different {
			name = "concurrent_conflict"
		}
		t.Run(name, func(t *testing.T) {
			SQLConcurrent(t, f, different)
		})
	}

	for _, name := range []string{
		"current", "not_selected", "revision_changed", "draft", "withdrawn",
		"future", "expired_boundary", "expires_after",
		"missing_verification", "verification_revision", "wrong_article",
		"missing_effective", "missing_published", "wrong_version",
	} {
		t.Run("evidence_"+name, func(t *testing.T) {
			ref := f.rule(t)
			want := name == "current" || name == "expires_after"
			switch name {
			case "not_selected":
				SQLMust(t, db.Model(&model.KnowledgeDocument{}).
					Where("id = ?", ref.DocumentID).
					Update("current_version_id", nil).Error)
			case "revision_changed":
				ref.PublicationRevision++
			case "draft":
				SQLMust(t, db.Model(&model.RuleVersion{}).
					Where("id = ?", ref.VersionID).
					Update("status", model.RuleVersionStatusDraft).Error)
			case "withdrawn":
				SQLMust(t, db.Model(&model.RuleVersion{}).
					Where("id = ?", ref.VersionID).
					Update("withdrawn_at", SQLAt).Error)
			case "future":
				SQLMust(t, db.Model(&model.RuleVersion{}).
					Where("id = ?", ref.VersionID).
					Update("effective_at", SQLAt.Add(time.Second)).Error)
			case "expired_boundary":
				SQLMust(t, db.Model(&model.RuleVersion{}).
					Where("id = ?", ref.VersionID).
					Update("expires_at", SQLAt).Error)
			case "expires_after":
				SQLMust(t, db.Model(&model.RuleVersion{}).
					Where("id = ?", ref.VersionID).
					Update("expires_at", SQLAt.Add(time.Second)).Error)
			case "missing_verification":
				SQLMust(t, db.Where("version_id = ?", ref.VersionID).
					Delete(&model.RuleVerification{}).Error)
			case "verification_revision":
				SQLMust(t, db.Model(&model.RuleVerification{}).
					Where("version_id = ?", ref.VersionID).
					Update("verified_content_revision", 2).Error)
			case "wrong_article":
				ref.ArticleID = f.rule(t).ArticleID
			case "missing_effective":
				SQLMust(t, db.Model(&model.RuleVersion{}).
					Where("id = ?", ref.VersionID).
					Update("effective_at", nil).Error)
			case "missing_published":
				SQLMust(t, db.Model(&model.RuleVersion{}).
					Where("id = ?", ref.VersionID).
					Update("published_at", nil).Error)
			case "wrong_version":
				ref.VersionID = f.rule(t).VersionID
			}
			ok, err := r.CheckAppealEvidence(
				SQLContext(t),
				[]model.AppealRuleEvidence{ref},
				SQLAt,
			)
			SQLMust(t, err)
			if ok != want {
				t.Fatalf("evidence available=%v want=%v", ok, want)
			}
		})
	}

	t.Run("multiple_documents", func(t *testing.T) {
		first, second := f.rule(t), f.rule(t)
		ok, err := r.CheckAppealEvidence(
			SQLContext(t),
			[]model.AppealRuleEvidence{second, first},
			SQLAt,
		)
		SQLMust(t, err)
		if !ok {
			t.Fatal("valid reverse-ordered documents rejected")
		}
	})

	t.Run("publication_lock_and_recheck", func(t *testing.T) {
		SQLPublication(t, f)
	})
	for _, cancel := range []bool{false, true} {
		name := "conversation_lock_timeout"
		if cancel {
			name = "conversation_lock_cancel"
		}
		t.Run(name, func(t *testing.T) {
			SQLConversationLock(t, f, cancel)
		})
	}
}

type SQLResult struct {
	result *service.AppealCreateResult
	err    error
}

func SQLConcurrent(t *testing.T, f *SQLFixture, different bool) {
	ctx := SQLContext(t)
	commands := []service.AppealCreateCommand{f.command(t)}
	commands = append(commands, commands[0])
	if different {
		commands[1].Input.Reason += " other"
		var err error
		commands[1].RequestHash, err = service.AppealRequestHash(
			commands[1].ConversationID, commands[1].Input,
		)
		SQLMust(t, err)
	}

	repos := []*AppealTicketRepository{
		SQLRepo(t, SQLOpen(t, f.cfg)),
		SQLRepo(t, SQLOpen(t, f.cfg)),
	}
	ready := make(chan struct{}, 2)
	gate := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	defer release()

	check := func(
		ctx context.Context, e service.AppealEvidenceChecker, at time.Time,
	) (model.AppealEligibilitySnapshot, error) {
		ready <- struct{}{}
		select {
		case <-gate:
			return SQLCheck(ctx, e, at)
		case <-ctx.Done():
			return model.AppealEligibilitySnapshot{}, ctx.Err()
		}
	}
	results := []chan SQLResult{
		make(chan SQLResult, 1),
		make(chan SQLResult, 1),
	}
	for i := range repos {
		go func(i int) {
			value, err := repos[i].CreateWithRecord(ctx, commands[i], check)
			results[i] <- SQLResult{value, err}
		}(i)
	}
	for i := 0; i < 2; i++ {
		select {
		case <-ready:
		case <-ctx.Done():
			t.Fatal("both requests did not reach pre-insert barrier")
		}
	}
	release()

	out := make([]SQLResult, 2)
	for i := range results {
		select {
		case out[i] = <-results[i]:
		case <-ctx.Done():
			t.Fatal("concurrent requests did not finish")
		}
	}
	for i := range out {
		if errors.Is(out[i].err, service.ErrAppealRetryableStorage) {
			t.Logf("MYSQL_CONCURRENCY participant=%d retryable=true bounded_retry=1", i)
			out[i].result, out[i].err = repos[i].
				CreateWithRecord(ctx, commands[i], SQLCheck)
		}
	}

	if different {
		success, conflict := 0, 0
		for _, v := range out {
			if v.err == nil && v.result != nil {
				success++
			} else if errors.Is(v.err, service.ErrAppealIdempotencyConflict) {
				conflict++
			} else {
				t.Fatalf("unexpected concurrency result: %v", v.err)
			}
		}
		if success != 1 || conflict != 1 {
			t.Fatalf("success=%d conflict=%d", success, conflict)
		}
	} else {
		for _, v := range out {
			SQLMust(t, v.err)
			if v.result == nil {
				t.Fatal("nil success")
			}
		}
		if out[0].result.TicketID != out[1].result.TicketID ||
			out[0].result.Replayed == out[1].result.Replayed {
			t.Fatal("expected one creation and one replay")
		}
	}
	SQLCounts(t, f.db, commands[0], 1, 1)
}

func SQLRuleCheck(
	ref model.AppealRuleEvidence,
) service.AppealTransactionCheck {
	return func(
		ctx context.Context, e service.AppealEvidenceChecker, at time.Time,
	) (model.AppealEligibilitySnapshot, error) {
		ok, err := e.CheckAppealEvidence(
			ctx, []model.AppealRuleEvidence{ref}, at,
		)
		if err != nil {
			return model.AppealEligibilitySnapshot{}, err
		}
		if !ok {
			return model.AppealEligibilitySnapshot{}, service.ErrAppealPreconditionChanged
		}
		s := SQLSnapshot(at)
		s.Decision = model.AppealEligibilityEligible
		s.ReasonCode = "test_only_eligible"
		s.PolicyCode, s.PolicyRevision = "TEST_ONLY", "v1"
		s.EvidenceRefs = []model.AppealRuleEvidence{ref}
		return service.NormalizeAppealSnapshot(s)
	}
}

func SQLPublication(t *testing.T, f *SQLFixture) {
	ctx := SQLContext(t)
	ref := f.rule(t)
	check := SQLRuleCheck(ref)

	snapshot := SQLSnapshot(SQLAt)
	snapshot.Decision = model.AppealEligibilityEligible
	snapshot.ReasonCode = "test_only_eligible"
	snapshot.PolicyCode, snapshot.PolicyRevision = "TEST_ONLY", "v1"
	snapshot.EvidenceRefs = []model.AppealRuleEvidence{ref}

	command := f.command(t)
	command.Input.IntakeMode = model.AppealIntakeStandard
	var err error
	command.RequestHash, err = service.AppealRequestHash(
		command.ConversationID, command.Input,
	)
	SQLMust(t, err)
	command.ExpectedPolicyDigest, err = service.AppealPolicyDigest(snapshot)
	SQLMust(t, err)

	locked := make(chan struct{})
	gate := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	defer release()
	done := make(chan SQLResult, 1)
	r := SQLRepo(t, SQLOpen(t, f.cfg))

	go func() {
		result, err := r.CreateWithRecord(ctx, command, func(
			ctx context.Context, e service.AppealEvidenceChecker, at time.Time,
		) (model.AppealEligibilitySnapshot, error) {
			s, err := check(ctx, e, at)
			if err != nil {
				return s, err
			}
			close(locked)
			select {
			case <-gate:
				return s, nil
			case <-ctx.Done():
				return model.AppealEligibilitySnapshot{}, ctx.Err()
			}
		})
		done <- SQLResult{result, err}
	}()
	select {
	case <-locked:
	case v := <-done:
		t.Fatalf("creation stopped before locking: %v", v.err)
	case <-ctx.Done():
		t.Fatal("rule lock not acquired")
	}

	writer := NewKnowledgeRepositoryWithClock(
		SQLOpen(t, f.cfg),
		func() time.Time { return SQLAt },
	)
	input := service.WithdrawKnowledgeVersionInput{
		DocumentID: ref.DocumentID, VersionID: ref.VersionID,
		ExpectedPublicationRevision: 1,
	}
	_, err = writer.WithdrawVersion(ctx, f.user.ID, input)
	SQLNumber(t, err, 1205)

	release()
	select {
	case v := <-done:
		SQLMust(t, v.err)
		if v.result == nil {
			t.Fatal("nil create result")
		}
	case <-ctx.Done():
		t.Fatal("creation failed to finish")
	}
	SQLCounts(t, f.db, command, 1, 1)

	_, err = writer.WithdrawVersion(ctx, f.user.ID, input)
	SQLMust(t, err)
	replayed, err := r.CreateWithRecord(ctx, command, nil)
	SQLMust(t, err)
	if !replayed.Replayed {
		t.Fatal("historical creation not replayed")
	}

	command.IdempotencyKey = SQLKey()
	if _, err = r.CreateWithRecord(ctx, command, check); !errors.Is(err, service.ErrAppealPreconditionChanged) {
		t.Fatalf("withdrawn policy used: %v", err)
	}
	SQLCounts(t, f.db, command, 0, 0)
}

func SQLConversationLock(
	t *testing.T, f *SQLFixture, cancel bool,
) {
	ctx := SQLContext(t)
	tx := f.db.WithContext(ctx).Begin()
	SQLMust(t, tx.Error)
	defer tx.Rollback()

	var conversation model.Conversation
	SQLMust(t, tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		First(&conversation, f.conversation.ID).Error)

	command := f.command(t)
	r := SQLRepo(t, SQLOpen(t, f.cfg))
	work := ctx
	if cancel {
		var stop context.CancelFunc
		work, stop = context.WithTimeout(ctx, 200*time.Millisecond)
		defer stop()
	}
	result, err := r.CreateWithRecord(work, command, SQLCheck)
	if result != nil {
		t.Fatal("blocked creation succeeded")
	}
	if cancel {
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("cancel=%v", err)
		}
	} else {
		if !errors.Is(err, service.ErrAppealRetryableStorage) {
			t.Fatalf("lock timeout=%v", err)
		}
		SQLNumber(t, err, 1205)
	}

	SQLMust(t, tx.Rollback().Error)
	SQLCounts(t, f.db, command, 0, 0)
	_, err = r.CreateWithRecord(ctx, command, SQLCheck)
	SQLMust(t, err)
	SQLCounts(t, f.db, command, 1, 1)
}
