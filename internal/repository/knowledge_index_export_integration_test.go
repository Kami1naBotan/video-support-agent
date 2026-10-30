//go:build indexexportintegration

package repository

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	indexing "video-support-agent/internal/index"
	"video-support-agent/internal/model"

	driver "github.com/go-sql-driver/mysql"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	gormlogger "gorm.io/gorm/logger"
)

type exportMySQLFixture struct {
	document model.KnowledgeDocument
	category model.RuleCategory
	version  model.RuleVersion
	article  model.RuleArticle
	chunk    model.KnowledgeChunk
}

type exportMySQLSuite struct {
	db       *gorm.DB
	writer   *gorm.DB
	pool     *sql.DB
	name     string
	uuid     string
	at       time.Time
	sequence int
}

func exportMySQLMust(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func exportMySQLOpen(t *testing.T, name string) (*gorm.DB, *sql.DB) {
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
	exportMySQLMust(t, err)
	pool := sql.OpenDB(connector)
	pool.SetMaxOpenConns(3)
	pool.SetMaxIdleConns(3)

	t.Cleanup(func() {
		if err := pool.Close(); err != nil {
			t.Errorf("close test connection pool: %v", err)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	exportMySQLMust(t, pool.PingContext(ctx))

	db, err := gorm.Open(gormmysql.New(gormmysql.Config{
		Conn:                      pool,
		SkipInitializeWithVersion: true,
	}), &gorm.Config{
		DisableAutomaticPing: true,
		Logger:               gormlogger.Default.LogMode(gormlogger.Silent),
	})
	exportMySQLMust(t, err)
	return db, pool
}

func newExportMySQLSuite(t *testing.T) *exportMySQLSuite {
	t.Helper()

	if os.Getenv("RUN_INDEX_EXPORT_MYSQL") != "1" {
		t.Fatal("RUN_INDEX_EXPORT_MYSQL=1 is required; this test does not skip")
	}
	if os.Getenv("INDEX_EXPORT_MYSQL_ADDR") != "127.0.0.1:13308" {
		t.Fatal("this suite requires the isolated address 127.0.0.1:13308")
	}
	if os.Getenv("ES_TEST_MYSQL_ROOT_PASSWORD") == "" {
		t.Fatal("isolated MySQL password is required")
	}
	expectedUUID := os.Getenv("INDEX_EXPORT_EXPECT_SERVER_UUID")
	if strings.TrimSpace(expectedUUID) == "" ||
		expectedUUID != strings.TrimSpace(expectedUUID) {
		t.Fatal("independently verified server UUID is required")
	}

	_, admin := exportMySQLOpen(t, "")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var actualUUID, version string
	exportMySQLMust(t, admin.QueryRowContext(
		ctx, "SELECT @@server_uuid, VERSION()",
	).Scan(&actualUUID, &version))
	if actualUUID != expectedUUID {
		t.Fatal("server UUID mismatch; no database was created")
	}
	if !strings.HasPrefix(version, "8.4.") {
		t.Fatalf("expected MySQL 8.4, got %s", version)
	}

	var random [8]byte
	_, err := rand.Read(random[:])
	exportMySQLMust(t, err)
	name := "es_export_it_" + hex.EncodeToString(random[:])
	t.Logf("TEST_DATABASE=%s SERVER_UUID=%s MYSQL_VERSION=%s",
		name, actualUUID, version)

	// The identifier contains only this fixed prefix and generated hex.
	_, err = admin.ExecContext(ctx,
		"CREATE DATABASE `"+name+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci")
	exportMySQLMust(t, err)

	// Registered before data pools: they close before this cleanup runs.
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("PRESERVED_TEST_DATABASE=%s", name)
			return
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(
			context.Background(), 20*time.Second,
		)
		defer cleanupCancel()
		if _, err := admin.ExecContext(
			cleanupCtx, "DROP DATABASE `"+name+"`",
		); err != nil {
			t.Errorf("cleanup failed; database preserved: %s: %v", name, err)
			return
		}
		t.Logf("DROPPED_TEST_DATABASE=%s", name)
	})

	db, pool := exportMySQLOpen(t, name)
	writer, _ := exportMySQLOpen(t, name)

	exportMySQLMust(t, db.WithContext(ctx).
		Set("gorm:table_options", "ENGINE=InnoDB").
		AutoMigrate(
			&model.KnowledgeDocument{},
			&model.RuleCategory{},
			&model.RuleVersion{},
			&model.RuleArticle{},
			&model.KnowledgeChunk{},
		))

	var engines int
	exportMySQLMust(t, pool.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM information_schema.tables
		WHERE table_schema = ?
		  AND engine = 'InnoDB'
		  AND table_name IN (
		    'knowledge_documents', 'rule_categories', 'rule_versions',
		    'rule_articles', 'knowledge_chunks'
		  )
	`, name).Scan(&engines))
	if engines != 5 {
		t.Fatalf("expected five real InnoDB tables, got %d", engines)
	}

	var isolation string
	exportMySQLMust(t, pool.QueryRowContext(
		ctx, "SELECT @@transaction_isolation",
	).Scan(&isolation))
	if isolation != "READ-COMMITTED" {
		t.Fatalf("test session default must be READ-COMMITTED, got %s", isolation)
	}
	t.Logf("SESSION_DEFAULT_ISOLATION=%s", isolation)

	return &exportMySQLSuite{
		db:     db,
		writer: writer,
		pool:   pool,
		name:   name,
		uuid:   actualUUID,
		at:     time.Date(2026, 10, 29, 4, 0, 0, 123000000, time.UTC),
	}
}

func (s *exportMySQLSuite) reset(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// This connection belongs only to the database created by this suite.
	for _, table := range []string{
		"knowledge_chunks", "rule_articles", "rule_versions",
		"rule_categories", "knowledge_documents",
	} {
		exportMySQLMust(t, s.db.WithContext(ctx).
			Exec("DELETE FROM `"+table+"`").Error)
	}
}

func (s *exportMySQLSuite) fixture(t *testing.T) exportMySQLFixture {
	t.Helper()
	s.sequence++
	key := fmt.Sprintf("isolated_%d", s.sequence)
	effective := s.at.Add(-time.Hour)

	f := exportMySQLFixture{
		document: model.KnowledgeDocument{
			DocumentKey:         key,
			PublicationRevision: 1,
		},
		category: model.RuleCategory{
			Code: key, Name: "隔离测试分类",
		},
	}
	exportMySQLMust(t, s.db.Create(&f.document).Error)
	exportMySQLMust(t, s.db.Create(&f.category).Error)

	f.version = model.RuleVersion{
		DocumentID:  f.document.ID,
		CategoryID:  f.category.ID,
		Version:     "v1",
		Title:       "虚构测试规则",
		Content:     "版本完整正文，不作为片段正文",
		SourceURL:   "https://example.invalid/isolated-rule",
		Status:      model.RuleVersionStatusPublished,
		EffectiveAt: &effective,
	}
	exportMySQLMust(t, s.db.Omit(clause.Associations).Create(&f.version).Error)

	f.article = model.RuleArticle{
		VersionID:     f.version.ID,
		ArticleOrder:  1,
		SectionPath:   "测试章节",
		ArticleNumber: "第一条",
		Content:       "条款完整正文，不作为片段正文",
	}
	exportMySQLMust(t, s.db.Omit(clause.Associations).Create(&f.article).Error)

	f.chunk = model.KnowledgeChunk{
		ArticleID: f.article.ID, ChunkIndex: 0, Content: "初始测试片段",
	}
	exportMySQLMust(t, s.db.Omit(clause.Associations).Create(&f.chunk).Error)
	exportMySQLMust(t, s.db.Model(&model.KnowledgeDocument{}).
		Where("id = ?", f.document.ID).
		Update("current_version_id", f.version.ID).Error)

	return f
}

func (s *exportMySQLSuite) export(
	ctx context.Context,
) (*indexing.Snapshot, error) {
	return NewKnowledgeRepositoryWithClock(
		s.db, func() time.Time { return s.at },
	).ExportCurrent(ctx)
}

// This callback only observes or schedules work before the real detail SQL.
// It never supplies query results or replaces the database transaction.
func (s *exportMySQLSuite) detailHook(
	t *testing.T,
	hook func(*gorm.DB) error,
) *int {
	t.Helper()
	count := new(int)
	name := "test:index_export_detail"

	exportMySQLMust(t, s.db.Callback().Row().
		Before("gorm:row").Register(name, func(tx *gorm.DB) {
		if !strings.Contains(tx.Statement.SQL.String(), "AS existing_category_id") {
			return
		}
		*count++
		if hook != nil {
			if err := hook(tx); err != nil {
				tx.AddError(err)
			}
		}
	}))
	t.Cleanup(func() {
		if err := s.db.Callback().Row().Remove(name); err != nil {
			t.Errorf("remove test callback: %v", err)
		}
	})
	return count
}

func exportMySQLIDs(snapshot *indexing.Snapshot) []string {
	ids := make([]string, len(snapshot.Documents))
	for i, document := range snapshot.Documents {
		ids[i] = document.ChunkID
	}
	return ids
}

func TestIndexExportMySQL(t *testing.T) {
	s := newExportMySQLSuite(t)

	run := func(name string, check func(*testing.T)) bool {
		return t.Run(name, func(t *testing.T) {
			s.reset(t)
			check(t)
		})
	}

	if !run("identity_empty_and_projection", func(t *testing.T) {
		empty, err := s.export(context.Background())
		exportMySQLMust(t, err)
		if empty.SourceDatabase != s.name ||
			empty.SourceServerUUID != s.uuid ||
			empty.CapturedAt != s.at ||
			empty.Documents == nil || len(empty.Documents) != 0 {
			t.Fatalf("incorrect empty snapshot: %+v", empty)
		}

		f := s.fixture(t)
		snapshot, err := s.export(context.Background())
		exportMySQLMust(t, err)
		if len(snapshot.Documents) != 1 {
			t.Fatalf("expected one document, got %d", len(snapshot.Documents))
		}
		d := snapshot.Documents[0]
		if d.DocumentID != strconv.FormatUint(f.document.ID, 10) ||
			d.VersionID != strconv.FormatUint(f.version.ID, 10) ||
			d.ArticleID != strconv.FormatUint(f.article.ID, 10) ||
			d.ChunkID != strconv.FormatUint(f.chunk.ID, 10) ||
			d.CategoryID != strconv.FormatUint(f.category.ID, 10) ||
			d.PublicationRevision != "1" ||
			d.Content != f.chunk.Content ||
			d.Title != f.version.Title ||
			d.SourceURL != f.version.SourceURL ||
			d.ArticleNumber != f.article.ArticleNumber ||
			d.ExpiresAt != nil || d.WithdrawnAt != nil {
			t.Fatalf("incorrect database projection: %+v", d)
		}
	}) {
		return
	}

	filters := []struct {
		name    string
		column  string
		value   any
		visible bool
	}{
		{"effective_equal", "effective_at", s.at, true},
		{"effective_future", "effective_at", s.at.Add(time.Millisecond), false},
		{"effective_missing", "effective_at", nil, false},
		{"expiry_equal", "expires_at", s.at, false},
		{"expiry_before", "expires_at", s.at.Add(-time.Millisecond), false},
		{"expiry_after", "expires_at", s.at.Add(time.Millisecond), true},
		{"draft", "status", model.RuleVersionStatusDraft, false},
		{"withdrawn_status", "status", model.RuleVersionStatusWithdrawn, false},
		{"withdrawn_timestamp", "withdrawn_at", s.at, false},
	}
	for _, tc := range filters {
		if !run(tc.name, func(t *testing.T) {
			f := s.fixture(t)
			exportMySQLMust(t, s.db.Model(&model.RuleVersion{}).
				Where("id = ?", f.version.ID).
				UpdateColumn(tc.column, tc.value).Error)
			got, err := s.export(context.Background())
			exportMySQLMust(t, err)
			want := 0
			if tc.visible {
				want = 1
			}
			if len(got.Documents) != want {
				t.Fatalf("expected %d visible fragments, got %d", want, len(got.Documents))
			}
		}) {
			return
		}
	}

	if !run("no_current_pointer", func(t *testing.T) {
		f := s.fixture(t)
		exportMySQLMust(t, s.db.Model(&model.KnowledgeDocument{}).
			Where("id = ?", f.document.ID).
			UpdateColumn("current_version_id", nil).Error)
		got, err := s.export(context.Background())
		exportMySQLMust(t, err)
		if len(got.Documents) != 0 {
			t.Fatal("published version without current pointer was exported")
		}
	}) {
		return
	}

	if !run("stable_article_and_chunk_order", func(t *testing.T) {
		f := s.fixture(t)
		exportMySQLMust(t, s.db.Model(&model.RuleArticle{}).
			Where("id = ?", f.article.ID).UpdateColumn("article_order", 3).Error)

		article := model.RuleArticle{
			VersionID: f.version.ID, ArticleOrder: 1,
			SectionPath: "", ArticleNumber: "", Content: "测试条款",
		}
		exportMySQLMust(t, s.db.Omit(clause.Associations).Create(&article).Error)
		later := model.KnowledgeChunk{
			ArticleID: article.ID, ChunkIndex: 3, Content: "后片段",
		}
		earlier := model.KnowledgeChunk{
			ArticleID: article.ID, ChunkIndex: 0, Content: "前片段",
		}
		exportMySQLMust(t, s.db.Omit(clause.Associations).Create(&later).Error)
		exportMySQLMust(t, s.db.Omit(clause.Associations).Create(&earlier).Error)

		got, err := s.export(context.Background())
		exportMySQLMust(t, err)
		want := []string{
			strconv.FormatUint(earlier.ID, 10),
			strconv.FormatUint(later.ID, 10),
			strconv.FormatUint(f.chunk.ID, 10),
		}
		if !reflect.DeepEqual(exportMySQLIDs(got), want) {
			t.Fatalf("incorrect order: %v", exportMySQLIDs(got))
		}
		if got.Documents[0].ArticleNumber != "" {
			t.Fatal("article number was invented")
		}
	}) {
		return
	}

	broken := []struct {
		name   string
		change func(*testing.T, exportMySQLFixture)
	}{
		{"dangling_pointer", func(t *testing.T, f exportMySQLFixture) {
			exportMySQLMust(t, s.db.Model(&model.KnowledgeDocument{}).
				Where("id = ?", f.document.ID).
				UpdateColumn("current_version_id", uint64(999999999)).Error)
		}},
		{"foreign_current_version", func(t *testing.T, f exportMySQLFixture) {
			other := s.fixture(t)
			exportMySQLMust(t, s.db.Model(&model.KnowledgeDocument{}).
				Where("id = ?", f.document.ID).
				UpdateColumn("current_version_id", other.version.ID).Error)
		}},
		{"missing_chunks", func(t *testing.T, f exportMySQLFixture) {
			exportMySQLMust(t, s.db.Where("article_id = ?", f.article.ID).
				Delete(&model.KnowledgeChunk{}).Error)
		}},
		{"missing_articles", func(t *testing.T, f exportMySQLFixture) {
			exportMySQLMust(t, s.db.Where("article_id = ?", f.article.ID).
				Delete(&model.KnowledgeChunk{}).Error)
			exportMySQLMust(t, s.db.Where("id = ?", f.article.ID).
				Delete(&model.RuleArticle{}).Error)
		}},
		{"missing_category", func(t *testing.T, f exportMySQLFixture) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			conn, err := s.pool.Conn(ctx)
			exportMySQLMust(t, err)
			defer conn.Close()

			_, err = conn.ExecContext(ctx, "SET FOREIGN_KEY_CHECKS=0")
			exportMySQLMust(t, err)
			defer func() {
				_, restoreErr := conn.ExecContext(
					context.Background(), "SET FOREIGN_KEY_CHECKS=1",
				)
				if restoreErr != nil {
					t.Errorf("restore test connection foreign keys: %v", restoreErr)
				}
			}()
			_, err = conn.ExecContext(ctx,
				"UPDATE rule_versions SET category_id=? WHERE id=?",
				uint64(999999999), f.version.ID)
			exportMySQLMust(t, err)
		}},
		{"invalid_later_row", func(t *testing.T, f exportMySQLFixture) {
			other := s.fixture(t)
			exportMySQLMust(t, s.db.Model(&model.KnowledgeChunk{}).
				Where("id = ?", other.chunk.ID).UpdateColumn("content", " ").Error)
		}},
	}
	for _, tc := range broken {
		if !run(tc.name, func(t *testing.T) {
			f := s.fixture(t)
			tc.change(t, f)
			got, err := s.export(context.Background())
			if got != nil || !errors.Is(err, indexing.ErrInvalidSource) {
				t.Fatalf("broken source returned partial/successful output: %v", err)
			}
		}) {
			return
		}
	}

	limits := []string{"single_text", "row_count", "total_text"}
	for _, kind := range limits {
		if !run("limit_"+kind, func(t *testing.T) {
			f := s.fixture(t)

			switch kind {
			case "single_text":
				exportMySQLMust(t, s.db.Model(&model.KnowledgeChunk{}).
					Where("id = ?", f.chunk.ID).
					UpdateColumn("content",
						strings.Repeat("x", indexing.MaxIndexDocumentJSONBytes+1)).Error)

			case "row_count":
				chunks := make([]model.KnowledgeChunk, indexing.MaxExportDocuments)
				for i := range chunks {
					chunks[i] = model.KnowledgeChunk{
						ArticleID:  f.article.ID,
						ChunkIndex: uint32(i + 1),
						Content:    "测试",
					}
				}
				exportMySQLMust(t, s.db.Omit(clause.Associations).
					CreateInBatches(&chunks, 500).Error)

			case "total_text":
				content := strings.Repeat("x", 600000)
				exportMySQLMust(t, s.db.Model(&model.KnowledgeChunk{}).
					Where("id = ?", f.chunk.ID).UpdateColumn("content", content).Error)
				chunks := make([]model.KnowledgeChunk, 112)
				for i := range chunks {
					chunks[i] = model.KnowledgeChunk{
						ArticleID:  f.article.ID,
						ChunkIndex: uint32(i + 1),
						Content:    content,
					}
				}
				exportMySQLMust(t, s.db.Omit(clause.Associations).
					CreateInBatches(&chunks, 8).Error)
			}

			detailCalls := s.detailHook(t, nil)
			got, err := s.export(context.Background())
			if got != nil || !errors.Is(err, indexing.ErrExportLimitExceeded) {
				t.Fatalf("resource overflow was not rejected: %v", err)
			}
			if *detailCalls != 0 {
				t.Fatal("resource overflow was detected only after reading detail rows")
			}
		}) {
			return
		}
	}

	if !run("json_expansion_after_database_export", func(t *testing.T) {
		f := s.fixture(t)
		exportMySQLMust(t, s.db.Model(&model.KnowledgeChunk{}).
			Where("id = ?", f.chunk.ID).
			UpdateColumn("content", strings.Repeat("\"", 600000)).Error)

		snapshot, err := s.export(context.Background())
		exportMySQLMust(t, err)
		if len(snapshot.Documents) != 1 {
			t.Fatal("valid raw text export was lost")
		}
		prepared, err := indexing.PrepareSnapshot(context.Background(), snapshot)
		if prepared != nil || !errors.Is(err, indexing.ErrExportLimitExceeded) {
			t.Fatalf("expanded JSON budget was not enforced: %v", err)
		}
	}) {
		return
	}

	if !run("transaction_is_read_only", func(t *testing.T) {
		f := s.fixture(t)
		calls := s.detailHook(t, func(tx *gorm.DB) error {
			err := tx.Session(&gorm.Session{NewDB: true}).
				Exec(`UPDATE knowledge_documents
				      SET publication_revision=publication_revision+1
				      WHERE id=?`, f.document.ID).Error
			var mysqlErr *driver.MySQLError
			if !errors.As(err, &mysqlErr) || mysqlErr.Number != 1792 {
				return fmt.Errorf("expected MySQL read-only error 1792, got %v", err)
			}
			return nil
		})

		got, err := s.export(context.Background())
		exportMySQLMust(t, err)
		if *calls != 1 || len(got.Documents) != 1 ||
			got.Documents[0].PublicationRevision != "1" {
			t.Fatal("read-only transaction evidence missing")
		}
		var revision uint64
		exportMySQLMust(t, s.writer.Model(&model.KnowledgeDocument{}).
			Where("id = ?", f.document.ID).
			Select("publication_revision").Scan(&revision).Error)
		if revision != 1 {
			t.Fatal("attempted write unexpectedly changed the database")
		}
		t.Log("READ_ONLY_WRITE_REJECTED=1792")
	}) {
		return
	}

	if !run("repeatable_read_during_committed_publication", func(t *testing.T) {
		f := s.fixture(t)

		next := f.version
		next.ID = 0
		next.Version = "v2"
		next.Title = "新测试版本"
		exportMySQLMust(t, s.writer.Omit(clause.Associations).Create(&next).Error)

		article := model.RuleArticle{
			VersionID: next.ID, ArticleOrder: 1,
			SectionPath: "新章节", ArticleNumber: "第二条", Content: "新条款",
		}
		exportMySQLMust(t, s.writer.Omit(clause.Associations).Create(&article).Error)
		chunks := []model.KnowledgeChunk{
			{ArticleID: article.ID, ChunkIndex: 0, Content: "新片段一"},
			{ArticleID: article.ID, ChunkIndex: 1, Content: "新片段二"},
		}
		exportMySQLMust(t, s.writer.Omit(clause.Associations).Create(&chunks).Error)

		committed := false
		calls := s.detailHook(t, func(tx *gorm.DB) error {
			if committed {
				return nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			err := s.writer.WithContext(ctx).Transaction(func(writer *gorm.DB) error {
				return writer.Model(&model.KnowledgeDocument{}).
					Where("id = ?", f.document.ID).
					UpdateColumns(map[string]any{
						"current_version_id":   next.ID,
						"publication_revision": 2,
					}).Error
			})
			if err == nil {
				committed = true
			}
			return err
		})

		first, err := s.export(context.Background())
		exportMySQLMust(t, err)
		if !committed || *calls != 1 ||
			len(first.Documents) != 1 ||
			first.Documents[0].VersionID != strconv.FormatUint(f.version.ID, 10) ||
			first.Documents[0].PublicationRevision != "1" ||
			first.Documents[0].Content != f.chunk.Content {
			t.Fatal("export mixed old statistics with newly committed source state")
		}

		second, err := s.export(context.Background())
		exportMySQLMust(t, err)
		if len(second.Documents) != 2 {
			t.Fatalf("next export did not observe committed version: %d", len(second.Documents))
		}
		for _, document := range second.Documents {
			if document.VersionID != strconv.FormatUint(next.ID, 10) ||
				document.PublicationRevision != "2" {
				t.Fatal("next export retained historical version data")
			}
		}
		t.Log("RR_SNAPSHOT=old_version_1_fragment NEXT_EXPORT=new_version_2_fragments")
	}) {
		return
	}

	if !run("cancellation_releases_transaction", func(t *testing.T) {
		s.fixture(t)
		s.pool.SetMaxOpenConns(1)
		defer s.pool.SetMaxOpenConns(3)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		fired := false
		s.detailHook(t, func(tx *gorm.DB) error {
			if !fired {
				fired = true
				cancel()
			}
			return nil
		})

		got, err := s.export(ctx)
		if !fired || got != nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("in-flight cancellation not preserved: %v", err)
		}

		// With only one pool slot, a leaked transaction blocks this export.
		recoveryCtx, recoveryCancel := context.WithTimeout(
			context.Background(), 10*time.Second,
		)
		defer recoveryCancel()
		recovered, err := s.export(recoveryCtx)
		exportMySQLMust(t, err)
		if len(recovered.Documents) != 1 {
			t.Fatal("connection pool did not recover after cancellation")
		}
		t.Log("CANCELLATION_RECOVERY=PASS")
	}) {
		return
	}
}
