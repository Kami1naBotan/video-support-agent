//go:build apimysql

package main

import (
	"context"
	"errors"
	driver "github.com/go-sql-driver/mysql"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"net"
	"os"
	"strings"
	"testing"
	"time"
	"video-support-agent/internal/config"
	"video-support-agent/internal/embedding"
	"video-support-agent/internal/knowledgechunk"
	"video-support-agent/internal/knowledgeworker"
	"video-support-agent/internal/model"
	"video-support-agent/internal/repository"
	"video-support-agent/internal/retrieval"
)

func apiTestPtr[T any](v T) *T { return &v }
func TestKnowledgeStartupMySQL(t *testing.T) {
	if os.Getenv("TEST_ISOLATED") != "1" || os.Getenv("DB_HOST") != "127.0.0.1" || os.Getenv("DB_NAME") != "bili_support_test" || os.Getenv("TEST_MYSQL_SERVER_UUID") == "" {
		t.Fatal("isolated script required")
	}
	d := driver.NewConfig()
	d.Net = "tcp"
	d.Addr = net.JoinHostPort(os.Getenv("DB_HOST"), os.Getenv("DB_PORT"))
	d.User = os.Getenv("DB_USER")
	d.Passwd = os.Getenv("DB_PASSWORD")
	d.DBName = os.Getenv("DB_NAME")
	d.ParseTime = true
	d.Timeout = 5 * time.Second
	db, err := gorm.Open(mysql.Open(d.FormatDSN()), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal("open failed")
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var identity struct{ Name, UUID string }
	if err = db.Raw("SELECT DATABASE() AS name, @@server_uuid AS uuid").Scan(&identity).Error; err != nil {
		t.Fatal(err)
	}
	if identity.Name != d.DBName || identity.UUID != os.Getenv("TEST_MYSQL_SERVER_UUID") {
		t.Fatal("isolation mismatch")
	}
	cfg := config.Config{}
	cfg.Database.Name = d.DBName
	cfg.Embedding = config.EmbeddingConfig{URL: "http://127.0.0.1:11434", Model: embedding.ModelName, ModelDigest: strings.Repeat("a", 64), Dimensions: "1024", ContextLength: "8192", Timeout: "5s"}
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	reset := func() {
		must(db.Exec("DELETE FROM knowledge_index_controls").Error)
		must(db.Exec("DELETE FROM knowledge_index_bootstraps").Error)
		must(db.Exec("DELETE FROM knowledge_index_generations").Error)
		must(db.Create(&model.KnowledgeIndexControl{ID: 1, DesiredEpoch: 1}).Error)
		must(db.Create(&model.KnowledgeIndexBootstrap{ID: 1, Status: "pending"}).Error)
	}
	seed := func(empty bool) {
		now := time.Now().UTC().Truncate(time.Millisecond)
		id := "00000000-0000-4000-8000-000000000001"
		hash := strings.Repeat("b", 64)
		g := model.KnowledgeIndexGeneration{ID: id, SourceEpoch: 1, OwnerToken: id, Attempt: 1, Status: model.KnowledgeGenerationPublished, BuildPhase: knowledgeworker.PhaseSealed, Empty: &empty, SourceDatabase: &d.DBName, SourceServerUUID: &identity.UUID, SnapshotAt: &now, ManifestSHA256: &hash, EmbeddingModel: apiTestPtr(embedding.ModelName), EmbeddingDigest: &cfg.Embedding.ModelDigest, EmbeddingDimension: apiTestPtr(uint32(embedding.Dimensions)), EmbeddingServerVersion: apiTestPtr("test"), EmbeddingContextLength: apiTestPtr(uint32(embedding.ContextLength)), EmbeddingInputPolicy: apiTestPtr(embedding.InputPolicy), Normalization: apiTestPtr(knowledgeworker.VectorNormalization), ChunkStrategy: apiTestPtr(knowledgechunk.Strategy), ESStatus: model.KnowledgeTargetVerified, MilvusStatus: model.KnowledgeTargetVerified, ESManifestSHA256: &hash, MilvusManifestSHA256: &hash, VerifiedAt: &now, PublishedAt: &now}
		if !empty {
			g.ChunkCount = 1
			g.ESIndexName = apiTestPtr("knowledge_g_" + strings.ReplaceAll(id, "-", ""))
			g.MilvusCollection = apiTestPtr("rule_chunks_vec_v1_" + strings.ReplaceAll(id, "-", ""))
		}
		must(db.Create(&g).Error)
		must(db.Model(&model.KnowledgeIndexControl{}).Where("id = 1").Updates(map[string]any{"active_epoch": 1, "active_generation_id": id}).Error)
		must(db.Model(&model.KnowledgeIndexBootstrap{}).Where("id = 1").Updates(map[string]any{"status": "completed", "completed_at": now}).Error)
	}
	for _, status := range []string{"pending", "scheduled"} {
		t.Run(status, func(t *testing.T) {
			reset()
			must(db.Model(&model.KnowledgeIndexBootstrap{}).Where("id=1").Update("status", status).Error)
			ready, err := checkKnowledgeAPIStartup(ctx, db, cfg, identity.UUID)
			if err != nil || ready {
				t.Fatalf("ready=%v err=%v", ready, err)
			}
			r, _ := repository.NewKnowledgeActiveRepository(db)
			_, err = r.ResolveActive(ctx)
			if !errors.Is(err, retrieval.ErrIndexNotReady) {
				t.Fatal(err)
			}
		})
	}
	for _, empty := range []bool{false, true} {
		name := "active"
		if empty {
			name = "empty"
		}
		t.Run(name, func(t *testing.T) {
			reset()
			seed(empty)
			ready, err := checkKnowledgeAPIStartup(ctx, db, cfg, identity.UUID)
			if err != nil || !ready {
				t.Fatalf("ready=%v err=%v", ready, err)
			}
			r, _ := repository.NewKnowledgeActiveRepository(db)
			target, err := r.ResolveActive(ctx)
			if err != nil || target.Empty != empty {
				t.Fatalf("target=%+v err=%v", target, err)
			}
		})
	}
	cases := map[string]string{
		"missing_control":        "DELETE FROM knowledge_index_controls",
		"missing_bootstrap":      "DELETE FROM knowledge_index_bootstraps",
		"zero_desired":           "UPDATE knowledge_index_controls SET desired_epoch=0",
		"epoch_ahead":            "UPDATE knowledge_index_controls SET active_epoch=2",
		"dangling_active":        "UPDATE knowledge_index_controls SET active_generation_id='00000000-0000-4000-8000-000000000009'",
		"lease_half":             "UPDATE knowledge_index_controls SET owner_token='owner'",
		"bootstrap_inconsistent": "UPDATE knowledge_index_bootstraps SET status='pending'",
		"manifest":               "UPDATE knowledge_index_generations SET es_manifest_sha256=REPEAT('c',64)",
		"partial":                "UPDATE knowledge_index_generations SET milvus_status='built'",
		"digest":                 "UPDATE knowledge_index_generations SET embedding_digest=REPEAT('c',64)",
		"source":                 "UPDATE knowledge_index_generations SET source_database='wrong'",
		"uuid":                   "UPDATE knowledge_index_generations SET source_server_uuid='wrong'",
	}
	for name, sql := range cases {
		t.Run(name, func(t *testing.T) {
			reset()
			seed(false)
			must(db.Exec(sql).Error)
			ready, err := checkKnowledgeAPIStartup(ctx, db, cfg, identity.UUID)
			if err == nil || ready {
				t.Fatalf("accepted corrupt state: ready=%v err=%v", ready, err)
			}
		})
	}
	t.Run("wrong_server", func(t *testing.T) {
		reset()
		_, err := checkKnowledgeAPIStartup(ctx, db, cfg, "00000000-0000-0000-0000-000000000000")
		if err == nil {
			t.Fatal("wrong server accepted")
		}
	})
	t.Run("missing_schema_readonly", func(t *testing.T) {
		reset()
		must(db.Exec("RENAME TABLE knowledge_index_generations TO isolated_missing_generations").Error)
		defer func() {
			must(db.Exec("RENAME TABLE isolated_missing_generations TO knowledge_index_generations").Error)
		}()
		_, err := checkKnowledgeAPIStartup(ctx, db, cfg, identity.UUID)
		if err == nil {
			t.Fatal("missing table accepted")
		}
		if db.Migrator().HasTable("knowledge_index_generations") {
			t.Fatal("startup migrated schema")
		}
	})
	// Change committed state after the first SELECT. Later reads must use its RR snapshot.
	t.Run("repeatable_read", func(t *testing.T) {
		reset()
		seed(false)
		hook := "test:active_switch"
		switched := false
		must(db.Callback().Query().After("gorm:query").Register(hook, func(tx *gorm.DB) {
			if switched || tx.Statement.Table != "knowledge_index_controls" {
				return
			}
			switched = true
			must(db.Exec("UPDATE knowledge_index_generations SET embedding_digest=REPEAT('c',64)").Error)
		}))
		defer db.Callback().Query().Remove(hook)
		ready, err := checkKnowledgeAPIStartup(ctx, db, cfg, identity.UUID)
		if !switched || err != nil || !ready {
			t.Fatalf("snapshot switched=%v ready=%v err=%v", switched, ready, err)
		}
		ready, err = checkKnowledgeAPIStartup(ctx, db, cfg, identity.UUID)
		if err == nil || ready {
			t.Fatal("new transaction did not see committed corruption")
		}
	})
	t.Run("readonly_transaction", func(t *testing.T) {
		reset()
		seed(false)
		hook := "test:readonly_enforced"
		attempted := false
		must(db.Callback().Query().After("gorm:query").Register(hook, func(tx *gorm.DB) {
			if attempted || tx.Statement.Table != "knowledge_index_controls" {
				return
			}
			attempted = true
			_, writeErr := tx.Statement.ConnPool.ExecContext(ctx, "UPDATE knowledge_index_controls SET desired_epoch=9 WHERE id=1")
			var mysqlErr *driver.MySQLError
			if !errors.As(writeErr, &mysqlErr) || mysqlErr.Number != 1792 {
				t.Errorf("read-only enforcement error=%v", writeErr)
			}
		}))
		defer db.Callback().Query().Remove(hook)
		ready, err := checkKnowledgeAPIStartup(ctx, db, cfg, identity.UUID)
		if !attempted || err != nil || !ready {
			t.Fatalf("attempted=%v ready=%v err=%v", attempted, ready, err)
		}
		var epoch uint64
		must(db.Raw("SELECT desired_epoch FROM knowledge_index_controls WHERE id=1").Scan(&epoch).Error)
		if epoch != 1 {
			t.Fatal("startup transaction wrote state")
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		c, cancel := context.WithCancel(ctx)
		cancel()
		ready, err := checkKnowledgeAPIStartup(c, db, cfg, identity.UUID)
		if err == nil || ready {
			t.Fatal("cancel ignored")
		}
	})
}
