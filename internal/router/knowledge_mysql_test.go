package router

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"video-support-agent/internal/auth"
	"video-support-agent/internal/cache"
	"video-support-agent/internal/config"
	"video-support-agent/internal/database"
	"video-support-agent/internal/model"
	"video-support-agent/internal/repository"
	"video-support-agent/internal/service"

	"gorm.io/gorm"
)

type AllowLimiter struct{}

func (AllowLimiter) Allow(context.Context, uint64) (cache.RateLimitResult, error) {
	return cache.RateLimitResult{Allowed: true}, nil
}

var KnowledgeTables = []string{
	"knowledge_documents",
	"rule_versions",
	"rule_articles",
	"knowledge_chunks",
	"rule_verifications",
	"rule_operations",
}

func OpenTestDB(t *testing.T, cfg config.DatabaseConfig) *gorm.DB {
	t.Helper()
	if cfg.Name != "bili_support_test" {
		t.Fatal("refusing database other than bili_support_test")
	}
	db, err := database.OpenMySQL(cfg)
	Must(t, err)
	pool, err := db.DB()
	Must(t, err)
	t.Cleanup(func() {
		if err := pool.Close(); err != nil {
			t.Errorf("close test database: %v", err)
		}
	})

	var actual string
	Must(t, db.Raw("SELECT DATABASE()").Scan(&actual).Error)
	if actual != "bili_support_test" {
		t.Fatalf("actual database=%q; refusing writes", actual)
	}

	tables := append([]string{"users", "rule_categories"}, KnowledgeTables...)
	for _, name := range tables {
		var row struct {
			Engine    string
			TableType string
		}
		result := db.Raw(`
			SELECT ENGINE AS engine, TABLE_TYPE AS table_type
			FROM information_schema.TABLES
			WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?
		`, name).Scan(&row)
		Must(t, result.Error)
		if result.RowsAffected != 1 ||
			row.Engine != "InnoDB" || row.TableType != "BASE TABLE" {
			t.Fatalf("require existing InnoDB base table %s; got %+v", name, row)
		}
	}

	// 测试库不能通过触发器或跨库外键影响其他数据库。
	// 检查当前账号可见的测试库触发器和双向跨库外键。
	// 元数据可见范围受账号权限限制，不能替代外部隔离确认。
	var triggers int64
	Must(t, db.Raw(`
	SELECT COUNT(*)
	FROM information_schema.TRIGGERS
	WHERE TRIGGER_SCHEMA = DATABASE()
`).Scan(&triggers).Error)

	if triggers != 0 {
		t.Fatalf(
			"isolation precondition failed: visible test database triggers=%d",
			triggers,
		)
	}

	var crossSchemaFKs int64
	Must(t, db.Raw(`
	SELECT COUNT(*)
	FROM information_schema.KEY_COLUMN_USAGE
	WHERE REFERENCED_TABLE_SCHEMA IS NOT NULL
	  AND TABLE_SCHEMA <> REFERENCED_TABLE_SCHEMA
	  AND (
	    TABLE_SCHEMA = DATABASE()
	    OR REFERENCED_TABLE_SCHEMA = DATABASE()
	  )
`).Scan(&crossSchemaFKs).Error)

	if crossSchemaFKs != 0 {
		t.Fatalf(
			"isolation precondition failed: visible cross-schema foreign keys=%d",
			crossSchemaFKs,
		)
	}

	t.Logf(
		"isolation metadata checked: visible triggers=%d, visible cross-schema foreign keys=%d",
		triggers,
		crossSchemaFKs,
	)

	var revisionColumns int64
	Must(t, db.Raw(`
	SELECT COUNT(*)
	FROM information_schema.COLUMNS
	WHERE TABLE_SCHEMA = DATABASE()
	  AND TABLE_NAME = 'rule_versions'
	  AND COLUMN_NAME = 'content_revision'
	  AND DATA_TYPE = 'bigint'
	  AND COLUMN_TYPE LIKE '%unsigned%'
	  AND IS_NULLABLE = 'NO'
	  AND COLUMN_DEFAULT = '1'
`).Scan(&revisionColumns).Error)
	if revisionColumns != 1 {
		t.Fatal("require rule_versions.content_revision BIGINT UNSIGNED NOT NULL DEFAULT 1")
	}

	var snapshotColumns int64
	Must(t, db.Raw(`
	SELECT COUNT(*)
	FROM information_schema.COLUMNS
	WHERE TABLE_SCHEMA = DATABASE()
	  AND TABLE_NAME = 'rule_verifications'
	  AND COLUMN_NAME = 'content_snapshot'
	  AND DATA_TYPE = 'json'
	  AND IS_NULLABLE = 'NO'
`).Scan(&snapshotColumns).Error)
	if snapshotColumns != 1 {
		t.Fatal("require rule_verifications.content_snapshot JSON NOT NULL")
	}

	t.Log("revision and verification snapshot column checks passed")

	return db
}

func Counts(t *testing.T, db *gorm.DB) []int64 {
	t.Helper()
	result := make([]int64, len(KnowledgeTables))
	for i, table := range KnowledgeTables {
		Must(t, db.Table(table).Count(&result[i]).Error)
	}
	return result
}

// 只按本次随机文档键和本次账号/分类ID清理，不清空表。
// 如果出现无法归属的孤儿数据，测试报错，不能扩大删除范围掩盖失败。
func Cleanup(db *gorm.DB, keys []string, users, categories []uint64) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var documents, versions, articles []uint64

		// 先确定本次夹具的归属范围。
		if len(keys) > 0 {
			if err := tx.Model(&model.KnowledgeDocument{}).
				Where("document_key IN ?", keys).
				Pluck("id", &documents).Error; err != nil {
				return err
			}
		}

		if len(documents) > 0 {
			if err := tx.Model(&model.RuleVersion{}).
				Where("document_id IN ?", documents).
				Pluck("id", &versions).Error; err != nil {
				return err
			}
		}

		if len(versions) > 0 {
			if err := tx.Model(&model.RuleArticle{}).
				Where("version_id IN ?", versions).
				Pluck("id", &articles).Error; err != nil {
				return err
			}
		}

		if len(documents) > 0 {
			if err := tx.Where("document_id IN ?", documents).
				Delete(&model.RuleOperation{}).Error; err != nil {
				return err
			}

			// 仅清理本次夹具文档，先解除其当前版本选择。
			if err := tx.Model(&model.KnowledgeDocument{}).
				Where("id IN ?", documents).
				Update("current_version_id", nil).Error; err != nil {
				return err
			}
		}

		// 再按外键依赖顺序删除，只处理本次夹具。
		if len(versions) > 0 {
			if err := tx.Where("version_id IN ?", versions).
				Delete(&model.RuleVerification{}).Error; err != nil {
				return err
			}
		}

		if len(articles) > 0 {
			if err := tx.Where("article_id IN ?", articles).
				Delete(&model.KnowledgeChunk{}).Error; err != nil {
				return err
			}

			if err := tx.Where("id IN ?", articles).
				Delete(&model.RuleArticle{}).Error; err != nil {
				return err
			}
		}

		if len(versions) > 0 {
			if err := tx.Where("id IN ?", versions).
				Delete(&model.RuleVersion{}).Error; err != nil {
				return err
			}
		}

		if len(documents) > 0 {
			if err := tx.Where("id IN ?", documents).
				Delete(&model.KnowledgeDocument{}).Error; err != nil {
				return err
			}
		}

		if len(users) > 0 {
			if err := tx.Where("id IN ?", users).
				Delete(&model.User{}).Error; err != nil {
				return err
			}
		}

		if len(categories) > 0 {
			if err := tx.Where("id IN ?", categories).
				Delete(&model.RuleCategory{}).Error; err != nil {
				return err
			}
		}

		return nil
	})
}

func TestKnowledgeMySQL(t *testing.T) {
	if os.Getenv("RUN_MYSQL_TESTS") != "1" {
		t.Skip("MySQL NOT verified: set RUN_MYSQL_TESTS=1 for the dedicated acceptance run")
	}
	if os.Getenv("DB_NAME") != "bili_support_test" {
		t.Fatal("explicit DB_NAME=bili_support_test is required")
	}
	if os.Getenv("TEST_ISOLATED") != "1" {
		t.Fatal("stop other writers to this test DB, then set TEST_ISOLATED=1")
	}

	cfg := config.Load()
	db := OpenTestDB(t, cfg.Database)
	observer := OpenTestDB(t, cfg.Database) // 独立连接池，不共享外层事务。
	initial := Counts(t, observer)

	random := make([]byte, 12)
	_, err := rand.Read(random)
	Must(t, err)
	prefix := "-" + hex.EncodeToString(random)

	var keys []string
	var userIDs, categoryIDs []uint64
	t.Cleanup(func() {
		if err := Cleanup(observer, keys, userIDs, categoryIDs); err != nil {
			t.Errorf("fixture cleanup failed; do not delete unrelated data: %v", err)
			return
		}
		Equal(t, Counts(t, observer), initial)
	})

	category := model.RuleCategory{
		Code: prefix, Name: "虚构测试分类，非官方",
	}
	Must(t, db.Create(&category).Error)
	categoryIDs = append(categoryIDs, category.ID)

	// 先创建再删除一个自有分类，获得确实不存在的非零ID。
	missing := model.RuleCategory{Code: prefix + "-gone", Name: "temporary test category"}
	Must(t, db.Create(&missing).Error)
	categoryIDs = append(categoryIDs, missing.ID)
	Must(t, db.Delete(&missing).Error)
	var missingCount int64
	Must(t, observer.Model(&model.RuleCategory{}).
		Where("id = ?", missing.ID).Count(&missingCount).Error)
	Equal(t, missingCount, int64(0))

	next := 0
	input := func() service.CreateKnowledgeDraftInput {
		next++
		in := Input()
		in.DocumentKey = fmt.Sprintf("%s-%d", prefix, next)
		in.CategoryID = category.ID
		keys = append(keys, in.DocumentKey)
		return in
	}
	svc := service.NewKnowledgeService(repository.NewKnowledgeRepository(db))
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	create := func(t *testing.T, in service.CreateKnowledgeDraftInput) *model.RuleVersion {
		t.Helper()
		v, err := svc.CreateDraft(ctx, in)
		Must(t, err)
		if v == nil || v.ID == 0 || v.DocumentID == 0 {
			t.Fatalf("invalid saved result: %+v", v)
		}
		return v
	}
	read := func(t *testing.T, id uint64) *service.KnowledgeDraftDetail {
		t.Helper()
		d, err := svc.GetDraftByID(ctx, id)
		Must(t, err)
		if d == nil || d.Version == nil {
			t.Fatal("nil draft detail")
		}
		return d
	}

	t.Run("persistence_order_isolation_and_non_draft", func(t *testing.T) {
		before := Counts(t, observer)
		in := input()
		future := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
		in.EffectiveAt = &future
		v := create(t, in)

		after := Counts(t, observer)
		Equal(t, after, []int64{
			before[0] + 1,
			before[1] + 1,
			before[2] + 2,
			before[3] + 2,
			before[4],
			before[5],
		})
		var document model.KnowledgeDocument
		Must(t, observer.First(&document, v.DocumentID).Error)
		Equal(t, document.DocumentKey, in.DocumentKey)
		Equal(t, document.CurrentVersionID, (*uint64)(nil))
		Equal(t, document.PublicationRevision, uint64(0))

		detail := read(t, v.ID)
		Equal(t, detail.Version.DocumentID, document.ID)
		Equal(t, detail.Version.CategoryID, category.ID)
		Equal(t, detail.Version.Content, in.Content)
		Equal(t, detail.Version.SourceURL, in.SourceURL)
		Equal(t, detail.Version.Status, model.RuleVersionStatusDraft)
		if detail.Version.EffectiveAt == nil || !detail.Version.EffectiveAt.Equal(future) {
			t.Fatalf("effective_at not preserved: %v", detail.Version.EffectiveAt)
		}
		Equal(t, detail.Version.ExpiresAt, (*time.Time)(nil))
		Equal(t, len(detail.Articles), 2)

		for i, article := range detail.Articles {
			Equal(t, article.VersionID, v.ID)
			Equal(t, article.ArticleOrder, uint32(i+1))
			Equal(t, article.SectionPath, in.Articles[i].SectionPath)
			Equal(t, article.ArticleNumber, in.Articles[i].ArticleNumber)
			Equal(t, article.Content, in.Articles[i].Content)
			var chunks []model.KnowledgeChunk
			Must(t, observer.Where("article_id = ?", article.ID).Find(&chunks).Error)
			Equal(t, len(chunks), 1)
			Equal(t, chunks[0].ChunkIndex, uint32(0))
			Equal(t, chunks[0].Content, article.Content)
		}

		// 外部文档的条款不能混入本版本。
		other := input()
		other.Articles[0].Content = "其他版本的虚构条款，不能混入"
		otherV := create(t, other)
		if otherV.ID == v.ID {
			t.Fatal("different drafts reused version ID")
		}

		firstID, secondID := detail.Articles[0].ID, detail.Articles[1].ID
		// 改成20、10，避免交换1、2时先触发唯一键冲突。
		Must(t, db.Model(&model.RuleArticle{}).
			Where("id = ?", firstID).Update("article_order", 20).Error)
		Must(t, db.Model(&model.RuleArticle{}).
			Where("id = ?", secondID).Update("article_order", 10).Error)
		sorted := read(t, v.ID)
		Equal(t, len(sorted.Articles), 2)
		Equal(t, sorted.Articles[0].ID, secondID)
		Equal(t, sorted.Articles[1].ID, firstID)
		for _, article := range sorted.Articles {
			Equal(t, article.VersionID, v.ID)
		}

		for _, status := range []model.RuleVersionStatus{
			model.RuleVersionStatusPublished, model.RuleVersionStatusWithdrawn,
		} {
			Must(t, db.Model(&model.RuleVersion{}).
				Where("id = ?", v.ID).Update("status", status).Error)
			got, err := svc.GetDraftByID(ctx, v.ID)
			if got != nil || !errors.Is(err, service.ErrKnowledgeDraftNotFound) {
				t.Fatalf("status=%s result=%+v error=%v", status, got, err)
			}
		}
		// 上面只构造读取隔离夹具，不代表发布或下线业务已验收。
	})

	t.Run("missing_category_and_duplicate_leave_no_extra_rows", func(t *testing.T) {
		in := input()
		in.CategoryID = missing.ID
		before := Counts(t, observer)
		got, err := svc.CreateDraft(ctx, in)
		if got != nil || !errors.Is(err, service.ErrRuleCategoryNotFound) {
			t.Fatalf("missing category: result=%+v error=%v", got, err)
		}
		Equal(t, Counts(t, observer), before)

		in = input()
		create(t, in)
		before = Counts(t, observer)
		got, err = svc.CreateDraft(ctx, in)
		if got != nil || !errors.Is(err, service.ErrKnowledgeDocumentExists) {
			t.Fatalf("duplicate: result=%+v error=%v", got, err)
		}
		Equal(t, Counts(t, observer), before)
	})

	t.Run("rollback_at_each_write_stage", func(t *testing.T) {
		cases := []struct {
			table string
			nth   int
		}{
			{"rule_versions", 1},
			{"rule_articles", 2},
			{"knowledge_chunks", 2},
		}
		for _, tc := range cases {
			t.Run(tc.table, func(t *testing.T) {
				in := input()
				before := Counts(t, observer)
				failure := errors.New("controlled create failure")
				callback := prefix + "-" + tc.table
				seen, injected := 0, false

				err := db.Callback().Create().Before("gorm:create").
					Register(callback, func(tx *gorm.DB) {
						if tx.Statement.Schema == nil ||
							tx.Statement.Schema.Table != tc.table {
							return
						}
						seen++
						if seen == tc.nth {
							injected = true
							tx.AddError(failure)
						}
					})
				Must(t, err)
				defer func() {
					if err := db.Callback().Create().Remove(callback); err != nil {
						t.Errorf("remove failure callback: %v", err)
					}
				}()

				got, err := svc.CreateDraft(ctx, in)
				if !injected {
					t.Fatal("fault injection did not execute; this is not rollback evidence")
				}
				if got != nil || !errors.Is(err, failure) {
					t.Fatalf("result=%+v error=%v; want injected failure", got, err)
				}
				Equal(t, Counts(t, observer), before)
				t.Logf("injected at %s occurrence %d; independent observer found no partial records",
					tc.table, tc.nth)
			})
		}
	})

	t.Run("repeatable_read_snapshot", func(t *testing.T) {
		in := input()
		v := create(t, in)
		old := read(t, v.ID)
		callback := prefix + "-snapshot"
		fired := false

		// 在读者读完版本之后、读条款之前，用另一个连接池提交修改。
		err := db.Callback().Query().After("gorm:query").
			Register(callback, func(tx *gorm.DB) {
				if fired || tx.Statement.Schema == nil ||
					tx.Statement.Schema.Table != "rule_versions" {
					return
				}
				fired = true
				writeErr := observer.WithContext(ctx).Transaction(func(w *gorm.DB) error {
					if err := w.Model(&model.RuleVersion{}).
						Where("id = ?", v.ID).Update("content", "新全文-虚构测试").Error; err != nil {
						return err
					}
					if err := w.Model(&model.RuleArticle{}).
						Where("version_id = ?", v.ID).
						Update("content", "新条款-虚构测试").Error; err != nil {
						return err
					}
					ids := make([]uint64, len(old.Articles))
					for i, article := range old.Articles {
						ids[i] = article.ID
					}
					return w.Model(&model.KnowledgeChunk{}).
						Where("article_id IN ?", ids).
						Update("content", "新条款-虚构测试").Error
				})
				if writeErr != nil {
					tx.AddError(fmt.Errorf("snapshot writer: %w", writeErr))
				}
			})
		Must(t, err)

		// 先读取并移除回调，再判断；避免失败时回调污染后续用例。
		detail, readErr := svc.GetDraftByID(ctx, v.ID)
		removeErr := db.Callback().Query().Remove(callback)
		Must(t, removeErr)
		Must(t, readErr)
		if !fired {
			t.Fatal("snapshot interleaving did not execute")
		}
		if detail == nil || detail.Version == nil {
			t.Fatal("nil snapshot result")
		}
		Equal(t, detail.Version.Content, old.Version.Content)
		Equal(t, len(detail.Articles), len(old.Articles))
		for i := range old.Articles {
			Equal(t, detail.Articles[i].ID, old.Articles[i].ID)
			Equal(t, detail.Articles[i].Content, old.Articles[i].Content)
		}

		newService := service.NewKnowledgeService(repository.NewKnowledgeRepository(observer))
		fresh, err := newService.GetDraftByID(ctx, v.ID)
		Must(t, err)
		Equal(t, fresh.Version.Content, "新全文-虚构测试")
		for _, article := range fresh.Articles {
			Equal(t, article.Content, "新条款-虚构测试")
		}
		t.Log("reader retained old version+articles; new transaction observed committed new content")
	})

	t.Run("actual_router_auth_roles_and_http_results", func(t *testing.T) {
		tokens, err := auth.NewTokenManager(
			strings.Repeat("k", 32), "test-only", time.Hour,
		)
		Must(t, err)
		roleTokens := map[string]string{}
		for _, role := range []string{model.RoleUser, model.RoleSupport, model.RoleAdmin} {
			user := model.User{
				Username:     prefix + "-" + role,
				PasswordHash: "test-fixture-not-a-login-password",
				Role:         role,
			}
			Must(t, db.Create(&user).Error)
			userIDs = append(userIDs, user.ID)
			token, err := tokens.Issue(user.ID)
			Must(t, err)
			roleTokens[role] = token
		}
		r := New(
			config.Config{AppName: "test"}, db, tokens,
			nil, AllowLimiter{}, nil, nil, nil,
		)
		base := "/api/v1/admin/knowledge"
		admin := roleTokens[model.RoleAdmin]

		in := input()
		body := JSON(t, Payload(in))
		w := Request(r, "POST", base+"/documents", body, admin, false)
		if w.Code != 201 {
			t.Fatalf("admin create: status=%d body=%s", w.Code, w.Body.String())
		}
		var created struct {
			DocumentID uint64 `json:"document_id"`
			VersionID  uint64 `json:"version_id"`
			Version    string `json:"version"`
			Status     string `json:"status"`
		}
		Must(t, json.Unmarshal(w.Body.Bytes(), &created))
		if created.DocumentID == 0 || created.VersionID == 0 ||
			created.Version != in.Version || created.Status != "draft" {
			t.Fatalf("unexpected create response: %+v", created)
		}
		WantJSON(t, w, 201, created)

		path := fmt.Sprintf("%s/versions/%d", base, created.VersionID)
		w = Request(r, "GET", path, "", admin, false)
		if w.Code != 200 {
			t.Fatalf("admin read: status=%d body=%s", w.Code, w.Body.String())
		}
		var detail struct {
			DocumentID uint64 `json:"document_id"`
			VersionID  uint64 `json:"version_id"`
			Content    string `json:"content"`
			SourceURL  string `json:"source_url"`
			Articles   []struct {
				ID      uint64 `json:"article_id"`
				Order   uint32 `json:"article_order"`
				Number  string `json:"article_number"`
				Content string `json:"content"`
			} `json:"articles"`
		}
		Must(t, json.Unmarshal(w.Body.Bytes(), &detail))
		Equal(t, detail.DocumentID, created.DocumentID)
		Equal(t, detail.VersionID, created.VersionID)
		Equal(t, detail.Content, in.Content)
		Equal(t, detail.SourceURL, in.SourceURL)
		Equal(t, len(detail.Articles), 2)
		for i, article := range detail.Articles {
			if article.ID == 0 {
				t.Fatal("missing article ID")
			}
			Equal(t, article.Order, uint32(i+1))
			Equal(t, article.Number, in.Articles[i].ArticleNumber)
			Equal(t, article.Content, in.Articles[i].Content)
		}
		t.Log("actual router: admin POST=201 GET=200; returned IDs and content match")

		before := Counts(t, observer)
		deniedBody := JSON(t, Payload(input()))
		for _, tc := range []struct {
			name, token, code string
			status            int
		}{
			{"anonymous", "", "unauthorized", 401},
			{"invalid_jwt", "invalid.jwt.token", "unauthorized", 401},
			{"user", roleTokens[model.RoleUser], "forbidden", 403},
			{"support", roleTokens[model.RoleSupport], "forbidden", 403},
		} {
			for _, method := range []string{"POST", "GET"} {
				t.Run(tc.name+"_"+method, func(t *testing.T) {
					target, requestBody := path, ""
					if method == "POST" {
						target, requestBody = base+"/documents", deniedBody
					}
					w := Request(r, method, target, requestBody, tc.token, false)
					WantError(t, w, tc.status, tc.code)
					if tc.status == 401 {
						Equal(t, w.Header().Get("WWW-Authenticate"), "Bearer")
					}
				})
			}
		}
		Equal(t, Counts(t, observer), before)

		w = Request(r, "POST", base+"/documents", body, admin, false)
		WantError(t, w, 409, "knowledge_document_exists")
		Equal(t, Counts(t, observer), before)

		missingInput := input()
		missingInput.CategoryID = missing.ID
		w = Request(r, "POST", base+"/documents",
			JSON(t, Payload(missingInput)), admin, false)
		WantError(t, w, 404, "rule_category_not_found")
		Equal(t, Counts(t, observer), before)

		invalid := input()
		invalid.CategoryID = 0
		w = Request(r, "POST", base+"/documents",
			JSON(t, Payload(invalid)), admin, false)
		WantError(t, w, 400, "invalid_argument")
		Equal(t, Counts(t, observer), before)

		w = Request(r, "GET", base+"/versions/0", "", admin, false)
		WantError(t, w, 400, "invalid_argument")

		// MAX(id)+1，并在查询前确认确实不存在；测试库禁止其他写入。
		var absent uint64
		Must(t, observer.Raw(
			"SELECT COALESCE(MAX(id), 0) + 1 FROM rule_versions",
		).Scan(&absent).Error)
		var count int64
		Must(t, observer.Model(&model.RuleVersion{}).
			Where("id = ?", absent).Count(&count).Error)
		Equal(t, count, int64(0))
		w = Request(r, "GET", fmt.Sprintf("%s/versions/%d", base, absent), "", admin, false)
		WantError(t, w, 404, "knowledge_draft_not_found")

		for _, status := range []model.RuleVersionStatus{
			model.RuleVersionStatusPublished, model.RuleVersionStatusWithdrawn,
		} {
			Must(t, db.Model(&model.RuleVersion{}).
				Where("id = ?", created.VersionID).Update("status", status).Error)
			w = Request(r, "GET", path, "", admin, false)
			WantError(t, w, 404, "knowledge_draft_not_found")
		}

		for _, unknown := range []bool{false, true} {
			largeInput := input()
			valid := JSON(t, Payload(largeInput))
			exact := valid + strings.Repeat(" ", 65536-len(valid))
			w = Request(r, "POST", base+"/documents", exact, admin, unknown)
			if w.Code != 201 {
				t.Fatalf("65536 bytes unknown=%v: status=%d body=%s",
					unknown, w.Code, w.Body.String())
			}
			before := Counts(t, observer)
			w = Request(r, "POST", base+"/documents", exact+" ", admin, unknown)
			WantError(t, w, 413, "request_too_large")
			Equal(t, Counts(t, observer), before)
		}
		t.Log("actual router verified 400/404/409/413; auth/role subtests verified 401/403")
	})
}

func TestKnowledgeMySQLPreflight(t *testing.T) {
	if os.Getenv("RUN_MYSQL_TESTS") != "1" {
		t.Fatal("preflight requires explicit RUN_MYSQL_TESTS=1")
	}
	if os.Getenv("DB_NAME") != "bili_support_test" {
		t.Fatal("preflight requires explicit DB_NAME=bili_support_test")
	}

	cfg := config.Load()
	db := OpenTestDB(t, cfg.Database)

	var identity struct {
		DatabaseName    string
		ServerUUID      string
		CurrentAccount  string
		SessionTimeZone string
	}
	Must(t, db.Raw(`
		SELECT
			DATABASE() AS database_name,
			@@server_uuid AS server_uuid,
			CURRENT_USER() AS current_account,
			@@session.time_zone AS session_time_zone
	`).Scan(&identity).Error)

	if identity.DatabaseName != "bili_support_test" {
		t.Fatalf("unexpected database: %q", identity.DatabaseName)
	}
	if identity.ServerUUID == "" || identity.CurrentAccount == "" {
		t.Fatalf("missing connection identity: %+v", identity)
	}

	t.Logf(
		"read-only preflight: database=%s server_uuid=%s account=%s session_timezone=%s",
		identity.DatabaseName,
		identity.ServerUUID,
		identity.CurrentAccount,
		identity.SessionTimeZone,
	)
	t.Log("no INSERT/UPDATE/DELETE/DDL executed by this preflight")
}
