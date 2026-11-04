//go:build auditmysql

package repository

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"video-support-agent/internal/config"
	"video-support-agent/internal/confirmation"
	"video-support-agent/internal/model"
	"video-support-agent/internal/service"

	driver "github.com/go-sql-driver/mysql"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

var auditAt = time.Date(
	2026, 11, 3, 10, 0, 0, 123000000, time.UTC,
)

func auditMust(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func auditWant(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("error=%v want=%v", err, want)
	}
}

func auditContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(
		context.Background(), 30*time.Second,
	)
	t.Cleanup(cancel)
	return ctx
}

func auditID(t *testing.T) string {
	t.Helper()
	id, err := service.NewAgentAuditID()
	auditMust(t, err)
	return id
}

func auditDatabase(t *testing.T) *gorm.DB {
	t.Helper()

	if os.Getenv("AGENT_AUDIT_MYSQL_ACCEPT") != "1" {
		t.Fatal("use the dedicated audit acceptance script")
	}

	host := os.Getenv("AGENT_AUDIT_MYSQL_HOST")
	port := os.Getenv("AGENT_AUDIT_MYSQL_PORT")
	user := os.Getenv("AGENT_AUDIT_MYSQL_USER")
	if host == "" || port == "" || user == "" {
		t.Fatal("missing explicit MySQL test target")
	}

	cfg := driver.NewConfig()
	cfg.Net = "tcp"
	cfg.Addr = net.JoinHostPort(host, port)
	cfg.User = user
	cfg.Passwd = os.Getenv("AGENT_AUDIT_MYSQL_PASSWORD")
	cfg.ParseTime = true
	cfg.Loc = time.UTC
	cfg.Timeout = 5 * time.Second
	cfg.ReadTimeout = 5 * time.Second
	cfg.WriteTimeout = 5 * time.Second
	cfg.Params = map[string]string{
		"innodb_lock_wait_timeout": "1",
		"time_zone":                "'+00:00'",
		"default_storage_engine":   "'InnoDB'",
		"sql_mode":                 "'STRICT_TRANS_TABLES,NO_ENGINE_SUBSTITUTION'",
	}

	admin, err := sql.Open("mysql", cfg.FormatDSN())
	auditMust(t, err)
	defer admin.Close()

	ctx := auditContext(t)
	var version string
	auditMust(t, admin.QueryRowContext(
		ctx, "SELECT VERSION()",
	).Scan(&version))

	if strings.Contains(strings.ToLower(version), "mariadb") ||
		!(strings.HasPrefix(version, "8.") ||
			strings.HasPrefix(version, "9.")) {
		t.Fatalf("requires MySQL 8/9, got %s", version)
	}

	name := "agent_audit_test_" +
		strings.ReplaceAll(auditID(t), "-", "")

	// 名称完全由程序生成；不接受已有库名，不使用IF NOT EXISTS。
	_, err = admin.ExecContext(
		ctx,
		"CREATE DATABASE "+name+
			" CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci",
	)
	auditMust(t, err)

	cfg.DBName = name
	db, err := gorm.Open(
		gormmysql.Open(cfg.FormatDSN()),
		&gorm.Config{
			Logger:               logger.Default.LogMode(logger.Silent),
			DisableAutomaticPing: true,
		},
	)
	auditMust(t, err)

	pool, err := db.DB()
	auditMust(t, err)
	pool.SetMaxOpenConns(8)
	pool.SetMaxIdleConns(4)
	t.Cleanup(func() {
		auditMust(t, pool.Close())
	})
	auditMust(t, pool.PingContext(ctx))

	t.Logf(
		"MYSQL_DATABASE name=%s version=%s retained=true",
		name, version,
	)
	return db
}

type auditFixture struct {
	t    *testing.T
	ctx  context.Context
	db   *gorm.DB
	repo *AgentAuditRepository
	svc  *service.AppealTicketService

	owner service.AgentAuditOwner
	other service.AgentAuditOwner
	now   time.Time
}

func auditSeed(t *testing.T, db *gorm.DB) *auditFixture {
	t.Helper()

	f := &auditFixture{
		t:   t,
		ctx: auditContext(t),
		db:  db,
		now: auditAt,
	}

	for _, owner := range []*service.AgentAuditOwner{
		&f.owner, &f.other,
	} {
		user := model.User{
			Username:     "TEST_" + auditID(t),
			PasswordHash: "NOT_A_LOGIN_HASH",
			Role:         model.RoleUser,
		}
		auditMust(t, db.WithContext(f.ctx).
			Omit(clause.Associations).Create(&user).Error)

		conversation := model.Conversation{
			UserID: user.ID,
			Title:  "TEST_ONLY_audit",
		}
		auditMust(t, db.WithContext(f.ctx).
			Omit(clause.Associations).Create(&conversation).Error)

		*owner = service.AgentAuditOwner{
			UserID:         user.ID,
			ConversationID: conversation.ID,
		}
	}

	var err error
	f.repo, err = NewAgentAuditRepositoryWithClock(
		db, func() time.Time { return f.now },
	)
	auditMust(t, err)

	store, err := NewAppealTicketRepositoryWithClock(
		db, func() time.Time { return f.now },
	)
	auditMust(t, err)

	eligibility, err := service.NewAppealEligibilityService(
		service.NoAppealPolicies{},
	)
	auditMust(t, err)

	signer, err := confirmation.NewAppealSignerWithClock(
		config.AppealConfirmationConfig{
			KeyBase64: base64.StdEncoding.EncodeToString(
				bytes.Repeat([]byte{0x73}, 32),
			),
			Issuer:   "test-audit",
			Audience: "test-audit-create",
		},
		func() time.Time { return f.now },
	)
	auditMust(t, err)

	f.svc, err = service.NewAppealTicketServiceWithClock(
		store,
		eligibility,
		signer,
		func() time.Time { return f.now },
	)
	auditMust(t, err)

	return f
}

func (f *auditFixture) run() *model.AgentRun {
	f.t.Helper()
	run, err := f.repo.StartRun(f.ctx, f.owner)
	auditMust(f.t, err)
	return run
}

func (f *auditFixture) call(
	runID, name, id string,
) *model.ToolCall {
	f.t.Helper()

	call, err := f.repo.StartCall(
		f.ctx,
		f.owner,
		runID,
		service.AgentCallStart{
			ToolName:      name,
			ModelCallID:   id,
			ArgumentBytes: 17,
		},
	)
	auditMust(f.t, err)
	return call
}

func auditReadFinish() service.AgentCallFinish {
	return service.AgentCallFinish{
		Status:         "succeeded",
		ErrorCode:      "none",
		BusinessEffect: "none",
	}
}

func auditRunFinish() service.AgentRunFinish {
	return service.AgentRunFinish{
		Status:     "completed",
		Decision:   "handoff",
		ReasonCode: "manual_review",
		ModelCalls: 2,
	}
}

func (f *auditFixture) read(
	id string,
) *service.AgentAuditSnapshot {
	f.t.Helper()
	snapshot, err := f.repo.ReadOwned(f.ctx, f.owner, id)
	auditMust(f.t, err)
	return snapshot
}

func (f *auditFixture) prepare() *service.PreparedAppeal {
	f.t.Helper()

	proposal, err := f.svc.Prepare(
		f.ctx,
		f.owner.UserID,
		f.owner.ConversationID,
		service.CreateAppealInput{
			SubjectType: model.AppealSubjectAccount,
			SubjectRef:  "TEST_PRIVATE_SUBJECT",
			Reason:      "TEST_PRIVATE_REASON",
			IntakeMode:  model.AppealIntakeManualReview,
		},
	)
	auditMust(f.t, err)
	return proposal
}

func (f *auditFixture) confirmed(
	run *model.AgentRun,
	proposal *service.PreparedAppeal,
) *model.ToolCall {
	f.t.Helper()

	call, err := f.repo.StartCall(
		f.ctx,
		f.owner,
		run.ID,
		service.AgentCallStart{
			ToolName:      "create_appeal_ticket",
			ArgumentBytes: 100,
			Binding:       &proposal.Binding,
		},
	)
	auditMust(f.t, err)
	return call
}

func (f *auditFixture) create(
	proposal *service.PreparedAppeal,
) *service.AppealCreateResult {
	f.t.Helper()

	result, err := f.svc.Create(
		f.ctx,
		f.owner.UserID,
		f.owner.ConversationID,
		proposal.Input,
		service.AppealConfirmationInput{
			IdempotencyKey: proposal.Binding.IdempotencyKey,
			PolicyDigest:   proposal.Binding.PolicyDigest,
			Token:          proposal.Token,
		},
	)
	auditMust(f.t, err)
	return result
}

func auditRace(t *testing.T, fn func() error) {
	t.Helper()

	ready := make(chan struct{}, 2)
	start := make(chan struct{})
	results := make(chan error, 2)

	for i := 0; i < 2; i++ {
		go func() {
			ready <- struct{}{}
			<-start
			results <- fn()
		}()
	}

	<-ready
	<-ready
	close(start)

	success, conflict := 0, 0
	for i := 0; i < 2; i++ {
		select {
		case err := <-results:
			switch {
			case err == nil:
				success++
			case errors.Is(err, service.ErrAgentAuditConflict):
				conflict++
			default:
				t.Errorf("race error: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("race did not finish")
		}
	}

	if success != 1 || conflict != 1 {
		t.Fatalf(
			"race success=%d conflict=%d",
			success, conflict,
		)
	}
}

func auditTrigger(t *testing.T, db *gorm.DB) {
	t.Helper()

	auditMust(t, db.Exec(
		"CREATE TRIGGER audit_test_fail "+
			"BEFORE UPDATE ON agent_runs FOR EACH ROW "+
			"SIGNAL SQLSTATE '45000' "+
			"SET MESSAGE_TEXT='TEST_ONLY_audit_failure'",
	).Error)

	t.Cleanup(func() {
		auditMust(t, db.Exec(
			"DROP TRIGGER audit_test_fail",
		).Error)
	})
}
