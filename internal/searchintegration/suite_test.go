//go:build searchintegration

package searchintegration

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"video-support-agent/internal/model"
	"video-support-agent/internal/repository"
	"video-support-agent/internal/retrieval"

	driver "github.com/go-sql-driver/mysql"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	gormlogger "gorm.io/gorm/logger"
)

type searchSuite struct {
	db     *gorm.DB
	writer *gorm.DB
	pool   *sql.DB
	name   string
	at     time.Time
	seq    int
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func runCase(t *testing.T, name string, fn func(*testing.T)) {
	t.Helper()
	if !t.Run(name, fn) {
		t.Fatal("停止后续场景，保留本次测试库")
	}
}

func openSearchDB(t *testing.T, name string) (*gorm.DB, *sql.DB) {
	t.Helper()
	cfg := driver.NewConfig()
	cfg.User = "root"
	cfg.Passwd = os.Getenv("ES_TEST_MYSQL_ROOT_PASSWORD")
	cfg.Net = "tcp"
	cfg.Addr = os.Getenv("INDEX_EXPORT_MYSQL_ADDR")
	cfg.DBName = name
	cfg.ParseTime = true
	cfg.Loc = time.UTC
	cfg.Timeout = 5 * time.Second
	cfg.ReadTimeout = 15 * time.Second
	cfg.WriteTimeout = 15 * time.Second
	cfg.Logger = &driver.NopLogger{}
	cfg.Params = map[string]string{
		"time_zone":             "'+00:00'",
		"transaction_isolation": "'READ-COMMITTED'",
	}
	connector, err := driver.NewConnector(cfg)
	must(t, err)
	pool := sql.OpenDB(connector)
	pool.SetMaxOpenConns(3)
	pool.SetMaxIdleConns(3)
	t.Cleanup(func() { must(t, pool.Close()) })

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	must(t, pool.PingContext(ctx))

	db, err := gorm.Open(gormmysql.New(gormmysql.Config{
		Conn: pool, SkipInitializeWithVersion: true,
	}), &gorm.Config{
		DisableAutomaticPing: true,
		Logger:               gormlogger.Default.LogMode(gormlogger.Silent),
	})
	must(t, err)
	return db, pool
}

func newSearchSuite(t *testing.T) *searchSuite {
	t.Helper()
	if os.Getenv("RUN_SEARCH_MYSQL") != "1" {
		t.Fatal("必须显式设置 RUN_SEARCH_MYSQL=1；不跳过测试")
	}
	if os.Getenv("INDEX_EXPORT_MYSQL_ADDR") != "127.0.0.1:13308" {
		t.Fatal("仅允许独立实例 127.0.0.1:13308")
	}
	if os.Getenv("ES_TEST_MYSQL_ROOT_PASSWORD") == "" {
		t.Fatal("缺少独立实例密码")
	}
	expected := os.Getenv("INDEX_EXPORT_EXPECT_SERVER_UUID")
	if !regexp.MustCompile(
		`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`,
	).MatchString(expected) {
		t.Fatal("缺少独立读取的有效实例 UUID")
	}

	_, admin := openSearchDB(t, "")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var uuid, version string
	must(t, admin.QueryRowContext(ctx,
		"SELECT @@server_uuid, VERSION()").Scan(&uuid, &version))
	if uuid != expected || !strings.HasPrefix(version, "8.4.") {
		t.Fatal("实例身份或版本不匹配；未创建测试库")
	}

	var random [8]byte
	_, err := rand.Read(random[:])
	must(t, err)
	name := "es_search_it_" + hex.EncodeToString(random[:])
	_, err = admin.ExecContext(ctx,
		"CREATE DATABASE `"+name+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci")
	must(t, err)
	t.Logf("TEST_DATABASE=%s SERVER_UUID=%s MYSQL_VERSION=%s", name, uuid, version)

	// 在数据连接池清理之前注册，清理时数据连接池会先关闭。
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("PRESERVED_TEST_DATABASE=%s", name)
			return
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cleanupCancel()
		_, err := admin.ExecContext(cleanupCtx, "DROP DATABASE `"+name+"`")
		if err != nil {
			t.Errorf("清理失败，保留测试库 %s: %v", name, err)
			return
		}
		t.Logf("DROPPED_TEST_DATABASE=%s", name)
	})

	db, pool := openSearchDB(t, name)
	writer, _ := openSearchDB(t, name)
	must(t, db.WithContext(ctx).Set("gorm:table_options", "ENGINE=InnoDB").
		AutoMigrate(
			&model.KnowledgeDocument{}, &model.RuleCategory{},
			&model.RuleVersion{}, &model.RuleArticle{},
			&model.KnowledgeChunk{}, &model.User{},
		))

	var engines int
	must(t, pool.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM information_schema.tables
		WHERE table_schema = ? AND engine = 'InnoDB'
		  AND table_name IN (
		    'knowledge_documents','rule_categories','rule_versions',
		    'rule_articles','knowledge_chunks','users'
		  )`, name).Scan(&engines))
	if engines != 6 {
		t.Fatalf("预期六张 InnoDB 表，实际 %d", engines)
	}

	var isolation string
	must(t, pool.QueryRowContext(ctx, "SELECT @@transaction_isolation").Scan(&isolation))
	if isolation != "READ-COMMITTED" {
		t.Fatalf("测试会话默认隔离级别应为 READ-COMMITTED，实际 %s", isolation)
	}
	t.Logf("SESSION_DEFAULT_ISOLATION=%s", isolation)

	return &searchSuite{
		db: db, writer: writer, pool: pool, name: name,
		at: time.Date(2026, 10, 30, 4, 0, 0, 123000000, time.UTC),
	}
}

func (s *searchSuite) fixture(t *testing.T) retrieval.Candidate {
	t.Helper()
	s.seq++
	key := fmt.Sprintf("search_fixture_%d", s.seq)
	document := model.KnowledgeDocument{
		DocumentKey: key, PublicationRevision: 1,
	}
	category := model.RuleCategory{Code: key, Name: "虚构测试分类"}
	must(t, s.db.Create(&document).Error)
	must(t, s.db.Create(&category).Error)

	effective := s.at.Add(-time.Hour)
	version := model.RuleVersion{
		DocumentID: document.ID, CategoryID: category.ID,
		Version: "测试版本", Title: "虚构测试规则",
		Content:     "仅用于隔离集成测试",
		SourceURL:   "https://example.invalid/search-test",
		Status:      model.RuleVersionStatusPublished,
		EffectiveAt: &effective,
	}
	must(t, s.db.Omit(clause.Associations).Create(&version).Error)
	article := model.RuleArticle{
		VersionID: version.ID, ArticleOrder: 1,
		SectionPath: "测试章节", ArticleNumber: "第一条",
		Content: "虚构测试条款",
	}
	must(t, s.db.Omit(clause.Associations).Create(&article).Error)
	chunk := model.KnowledgeChunk{
		ArticleID: article.ID, ChunkIndex: 0, Content: "虚构测试片段",
	}
	must(t, s.db.Omit(clause.Associations).Create(&chunk).Error)
	must(t, s.db.Model(&model.KnowledgeDocument{}).
		Where("id = ?", document.ID).
		Update("current_version_id", version.ID).Error)

	return retrieval.Candidate{
		PublicationRevision: 1,
		Chunk: retrieval.RuleChunk{
			ChunkID: chunk.ID, DocumentID: document.ID, VersionID: version.ID,
			ArticleID: article.ID, CategoryID: category.ID,
			ChunkIndex: 0, ArticleOrder: 1,
			Version: version.Version, Title: version.Title,
			SectionPath: article.SectionPath, ArticleNumber: article.ArticleNumber,
			Content: chunk.Content, SourceURL: version.SourceURL,
			EffectiveAt: effective, Score: 1,
		},
	}
}

func (s *searchSuite) verify(
	t *testing.T, candidates []retrieval.Candidate, want ...uint64,
) {
	t.Helper()
	got, err := repository.NewKnowledgeRepository(s.db).
		VerifyCurrent(context.Background(), candidates, s.at)
	must(t, err)
	if got == nil || len(got) != len(want) {
		t.Fatalf("复核集合不符：got=%v want=%v", got, want)
	}
	for _, id := range want {
		if _, ok := got[id]; !ok {
			t.Fatalf("缺少有效片段 %d，got=%v", id, got)
		}
	}
}

func (s *searchSuite) rowHook(
	t *testing.T, fn func(*gorm.DB) error,
) *int {
	t.Helper()
	count := new(int)
	const name = "test:search_current"
	must(t, s.db.Callback().Row().Before("gorm:row").Register(name, func(tx *gorm.DB) {
		if !strings.Contains(tx.Statement.SQL.String(), "c.id AS chunk_id") {
			return
		}
		(*count)++
		if fn != nil {
			tx.AddError(fn(tx))
		}
	}))
	t.Cleanup(func() { must(t, s.db.Callback().Row().Remove(name)) })
	return count
}

// 仅在本次自建库、同一条专用连接上构造断链数据，并恢复外键检查。
// 不修改生产 schema，不执行全局 SET。
func (s *searchSuite) removeParent(t *testing.T, table string, id uint64) {
	t.Helper()
	switch table {
	case "rule_articles", "rule_versions", "knowledge_documents", "rule_categories":
	default:
		t.Fatal("非法测试表名")
	}
	must(t, s.db.Connection(func(conn *gorm.DB) (err error) {
		if err = conn.Exec("SET SESSION FOREIGN_KEY_CHECKS=0").Error; err != nil {
			return err
		}
		defer func() {
			err = errors.Join(err, conn.Exec("SET SESSION FOREIGN_KEY_CHECKS=1").Error)
		}()
		return conn.Exec("DELETE FROM `"+table+"` WHERE id = ?", id).Error
	}))
}
