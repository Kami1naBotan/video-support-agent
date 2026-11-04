//go:build day11mysql

package repository

import (
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"video-support-agent/internal/database"
	"video-support-agent/internal/model"
	"video-support-agent/internal/service"

	driver "github.com/go-sql-driver/mysql"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

var day11SQLAt = time.Date(2026, 11, 3, 4, 0, 0, 123000000, time.UTC)

type day11SQLFixture struct {
	db                              *gorm.DB
	cfg                             *driver.Config
	user, other                     model.User
	conversation, otherConversation model.Conversation
}

func day11SQLMust(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func day11SQLContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func day11SQLKey() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x",
		b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func day11SQLOpen(t *testing.T, cfg *driver.Config) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(gormmysql.Open(cfg.FormatDSN()), &gorm.Config{
		Logger:               logger.Default.LogMode(logger.Silent),
		DisableAutomaticPing: true,
	})
	day11SQLMust(t, err)
	pool, err := db.DB()
	day11SQLMust(t, err)
	pool.SetMaxOpenConns(8)
	pool.SetMaxIdleConns(4)
	t.Cleanup(func() { day11SQLMust(t, pool.Close()) })
	day11SQLMust(t, pool.PingContext(day11SQLContext(t)))
	return db
}

func day11SQLDatabase(t *testing.T) (*gorm.DB, *driver.Config) {
	t.Helper()
	if os.Getenv("DAY11_MYSQL_ACCEPT") != "1" {
		t.Fatal("run the dedicated block3 script; database tests are not implicitly enabled")
	}
	host := os.Getenv("DAY11_MYSQL_HOST")
	port := os.Getenv("DAY11_MYSQL_PORT")
	user := os.Getenv("DAY11_MYSQL_USER")
	if host == "" || port == "" || user == "" {
		t.Fatal("DAY11_MYSQL_HOST/PORT/USER required")
	}

	cfg := driver.NewConfig()
	cfg.Net, cfg.Addr = "tcp", net.JoinHostPort(host, port)
	cfg.User, cfg.Passwd = user, os.Getenv("DAY11_MYSQL_PASSWORD")
	cfg.ParseTime, cfg.Loc = true, time.UTC
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
	day11SQLMust(t, err)
	defer admin.Close()

	ctx := day11SQLContext(t)
	day11SQLMust(t, admin.PingContext(ctx))
	var version string
	day11SQLMust(t,
		admin.QueryRowContext(ctx, "SELECT VERSION()").Scan(&version),
	)
	if strings.Contains(strings.ToLower(version), "mariadb") ||
		!(strings.HasPrefix(version, "8.") || strings.HasPrefix(version, "9.")) {
		t.Fatalf("this suite targets MySQL 8/9; observed %s", version)
	}

	name := "day11_test_" + strings.ReplaceAll(day11SQLKey(), "-", "")
	_, err = admin.ExecContext(ctx,
		"CREATE DATABASE `"+name+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci",
	)
	// 不使用IF NOT EXISTS，不接受业务库名，不复用已有库。
	day11SQLMust(t, err)

	cfg.DBName = name
	db := day11SQLOpen(t, cfg)
	t.Logf("MYSQL_DATABASE name=%s version=%s retained=true", name, version)
	return db, cfg
}

func day11SQLSeed(
	t *testing.T, db *gorm.DB, cfg *driver.Config,
) *day11SQLFixture {
	t.Helper()
	f := &day11SQLFixture{db: db, cfg: cfg}
	f.user = model.User{
		Username:     "TEST_ONLY_owner",
		PasswordHash: "NOT_A_LOGIN_HASH",
		Role:         model.RoleUser,
	}
	f.other = model.User{
		Username:     "TEST_ONLY_other",
		PasswordHash: "NOT_A_LOGIN_HASH",
		Role:         model.RoleUser,
	}
	for _, value := range []any{&f.user, &f.other} {
		day11SQLMust(t, db.Omit(clause.Associations).Create(value).Error)
	}
	f.conversation = model.Conversation{
		UserID: f.user.ID, Title: "TEST_ONLY_owner",
	}
	f.otherConversation = model.Conversation{
		UserID: f.other.ID, Title: "TEST_ONLY_other",
	}
	for _, value := range []any{&f.conversation, &f.otherConversation} {
		day11SQLMust(t, db.Omit(clause.Associations).Create(value).Error)
	}
	return f
}

func day11SQLRepo(t *testing.T, db *gorm.DB) *AppealTicketRepository {
	t.Helper()
	r, err := NewAppealTicketRepositoryWithClock(
		db, func() time.Time { return day11SQLAt },
	)
	day11SQLMust(t, err)
	return r
}

func day11SQLSnapshot(at time.Time) model.AppealEligibilitySnapshot {
	return model.AppealEligibilitySnapshot{
		SchemaVersion: 1,
		Decision:      model.AppealEligibilityManualReview,
		ReasonCode:    "policy_unavailable",
		CheckedAt:     at,
		FactSources: []model.AppealFactProvenance{
			{Field: "subject_type", Source: model.AppealFactUserReported},
			{Field: "subject_ref", Source: model.AppealFactUserReported},
			{Field: "reason", Source: model.AppealFactUserReported},
		},
	}
}

func day11SQLCheck(
	_ context.Context, _ service.AppealEvidenceChecker, at time.Time,
) (model.AppealEligibilitySnapshot, error) {
	return service.NormalizeAppealSnapshot(day11SQLSnapshot(at))
}

func (f *day11SQLFixture) command(t *testing.T) service.AppealCreateCommand {
	t.Helper()
	input := service.CreateAppealInput{
		SubjectType: model.AppealSubjectAccount,
		SubjectRef:  "TEST_ONLY_账号",
		Reason:      "测试申请\n保留换行",
		IntakeMode:  model.AppealIntakeManualReview,
	}
	hash, err := service.AppealRequestHash(f.conversation.ID, input)
	day11SQLMust(t, err)
	digest, err := service.AppealPolicyDigest(day11SQLSnapshot(day11SQLAt))
	day11SQLMust(t, err)
	return service.AppealCreateCommand{
		UserID:               f.user.ID,
		ConversationID:       f.conversation.ID,
		IdempotencyKey:       day11SQLKey(),
		RequestHash:          hash,
		ExpectedPolicyDigest: digest,
		Input:                input,
	}
}

func day11SQLCounts(
	t *testing.T, db *gorm.DB, c service.AppealCreateCommand,
	wantTickets, wantRecords int64,
) {
	t.Helper()
	var tickets, records int64
	day11SQLMust(t, db.Model(&model.AppealTicket{}).
		Where("user_id = ? AND idempotency_key = ?", c.UserID, c.IdempotencyKey).
		Count(&tickets).Error)
	day11SQLMust(t, db.Table("ticket_records AS r").
		Joins("JOIN appeal_tickets AS a ON a.id = r.ticket_id").
		Where("a.user_id = ? AND a.idempotency_key = ?", c.UserID, c.IdempotencyKey).
		Count(&records).Error)
	if tickets != wantTickets || records != wantRecords {
		t.Fatalf("rows tickets=%d records=%d want=%d/%d",
			tickets, records, wantTickets, wantRecords)
	}
	t.Logf("MYSQL_ROWS key=%s tickets=%d records=%d",
		c.IdempotencyKey, tickets, records)
}

func day11SQLLegacyModels() []any {
	return []any{
		&model.User{}, &model.Conversation{}, &model.Message{},
		&model.KnowledgeDocument{}, &model.RuleCategory{}, &model.RuleVersion{},
		&model.RuleArticle{}, &model.KnowledgeChunk{},
		&model.RuleVerification{}, &model.RuleOperation{},
	}
}

func day11SQLMigrate(t *testing.T, db *gorm.DB) {
	t.Helper()
	day11SQLMust(t, database.Migrate(db))
}

func (f *day11SQLFixture) rule(t *testing.T) model.AppealRuleEvidence {
	t.Helper()
	category := model.RuleCategory{Code: day11SQLKey(), Name: "TEST_ONLY"}
	doc := model.KnowledgeDocument{DocumentKey: day11SQLKey()}
	for _, value := range []any{&category, &doc} {
		day11SQLMust(t, f.db.Omit(clause.Associations).Create(value).Error)
	}

	at := day11SQLAt
	version := model.RuleVersion{
		DocumentID: doc.ID, CategoryID: category.ID,
		Version: "TEST_ONLY_v1", ContentRevision: 1,
		Status: model.RuleVersionStatusPublished,
		Title:  "TEST_ONLY", Content: "Fictional test data",
		SourceURL:   "https://example.invalid/test",
		EffectiveAt: &at, PublishedAt: &at,
	}
	day11SQLMust(t, f.db.Omit(clause.Associations).Create(&version).Error)

	article := model.RuleArticle{
		VersionID: version.ID, ArticleOrder: 1, Content: "TEST_ONLY",
	}
	verification := model.RuleVerification{
		VersionID: version.ID, VerifiedContentRevision: 1,
		VerifiedBy: f.user.ID, VerifiedAt: at,
		VerificationNote: "TEST_ONLY", ContentSnapshot: "{}",
	}
	for _, value := range []any{&article, &verification} {
		day11SQLMust(t, f.db.Omit(clause.Associations).Create(value).Error)
	}
	day11SQLMust(t, f.db.Model(&doc).Updates(map[string]any{
		"current_version_id":   version.ID,
		"publication_revision": 1,
	}).Error)

	return model.AppealRuleEvidence{
		DocumentID: doc.ID, VersionID: version.ID,
		ArticleID: article.ID, PublicationRevision: 1,
	}
}
