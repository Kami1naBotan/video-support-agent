package router

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"video-support-agent/internal/config"
	"video-support-agent/internal/database"
	"video-support-agent/internal/model"
	"video-support-agent/internal/repository"
	"video-support-agent/internal/service"

	mysqlDialect "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/schema"
)

// 在真实仓储提交之后、服务收到结果之前，插入另一次已提交的操作。
// 所有持久化仍由真实仓储完成；只控制两个操作的先后位置。
type knowledgeAfterCommitStore struct {
	*repository.KnowledgeRepository
	after func() error
}

func (s *knowledgeAfterCommitStore) EditDraftWithVerification(
	ctx context.Context,
	versionID uint64,
	expectedRevision uint64,
	content model.RuleVersion,
	articles []model.RuleArticle,
) (*model.RuleVersion, *model.RuleVerification, error) {
	version, verification, err :=
		s.KnowledgeRepository.EditDraftWithVerification(
			ctx, versionID, expectedRevision, content, articles,
		)
	if err != nil {
		return nil, nil, err
	}

	if s.after != nil {
		if err := s.after(); err != nil {
			return nil, nil, fmt.Errorf("test after-commit operation: %w", err)
		}
	}
	return version, verification, nil
}

func requireKnowledgeRemainingMySQL(t *testing.T) {
	t.Helper()
	if os.Getenv("RUN_MYSQL_TESTS") != "1" ||
		os.Getenv("DB_NAME") != "bili_support_test" ||
		os.Getenv("TEST_ISOLATED") != "1" {
		t.Fatal("require RUN_MYSQL_TESTS=1, DB_NAME=bili_support_test, TEST_ISOLATED=1")
	}
}

func TestKnowledgeRemainingMySQLEditResult(t *testing.T) {
	requireKnowledgeRemainingMySQL(t)

	cfg := config.Load()
	editorDB := OpenTestDB(t, cfg.Database)
	observer := OpenTestDB(t, cfg.Database)
	initial := Counts(t, observer)

	var random [12]byte
	_, err := rand.Read(random[:])
	Must(t, err)
	key := "test-edit-result-" + hex.EncodeToString(random[:])

	var userID, categoryID uint64
	t.Cleanup(func() {
		var users, categories []uint64
		if userID != 0 {
			users = []uint64{userID}
		}
		if categoryID != 0 {
			categories = []uint64{categoryID}
		}
		if err := Cleanup(observer, []string{key}, users, categories); err != nil {
			t.Errorf("cleanup failed: %v; do not delete unrelated data", err)
			return
		}
		Equal(t, Counts(t, observer), initial)
	})

	category := model.RuleCategory{
		Code: key, Name: "【虚构测试】编辑返回值分类",
	}
	Must(t, observer.Create(&category).Error)
	categoryID = category.ID

	user := model.User{
		Username: key, PasswordHash: "test-fixture-not-a-login-password",
		Role: model.RoleAdmin,
	}
	Must(t, observer.Create(&user).Error)
	userID = user.ID

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	editor := service.NewKnowledgeService(
		repository.NewKnowledgeRepository(editorDB),
	)
	writer := service.NewKnowledgeService(
		repository.NewKnowledgeRepository(observer),
	)

	version, err := editor.CreateDraft(ctx, service.CreateKnowledgeDraftInput{
		DocumentKey: key, Version: "test-v1", CategoryID: categoryID,
		Title:     "【虚构测试】编辑返回值",
		Content:   "【虚构测试】初始正文",
		SourceURL: "https://www.bilibili.com/test-only",
		Articles: []service.CreateRuleArticleInput{
			{
				SectionPath: "测试章节", ArticleNumber: "1",
				Content: "【虚构测试】初始条款",
			},
		},
	})
	Must(t, err)

	firstVerification, err := editor.VerifyDraft(
		ctx, userID, service.VerifyKnowledgeDraftInput{
			VersionID: version.ID, ExpectedContentRevision: 1,
			VerificationNote: "【虚构测试】初始核验",
		},
	)
	Must(t, err)

	change := service.EditKnowledgeDraftInput{
		VersionID: version.ID, ExpectedContentRevision: 1,
		CategoryID: categoryID, Title: "【虚构测试】编辑返回值",
		Content:   "【虚构测试】第二次内容",
		SourceURL: "https://www.bilibili.com/test-only",
		Articles: []service.CreateRuleArticleInput{
			{
				SectionPath: "测试章节", ArticleNumber: "1",
				Content: "【虚构测试】第二次条款",
			},
		},
	}

	// 编辑已更新版本和条款，但读取返回摘要失败时，也必须全部回滚。
	t.Run("summary_read_failure_rolls_back_edit", func(t *testing.T) {
		before := readVerificationState(t, observer, version.ID)
		countsBefore := Counts(t, observer)
		failure := errors.New("controlled verification summary read failure")
		callbackName := key + "-summary-failure"
		injected := false

		Must(t, editorDB.Callback().Query().Before("gorm:query").
			Register(callbackName, func(tx *gorm.DB) {
				if tx.Statement.Schema != nil &&
					tx.Statement.Schema.Table == "rule_verifications" {
					injected = true
					tx.AddError(failure)
				}
			}))
		defer func() {
			if err := editorDB.Callback().Query().Remove(callbackName); err != nil {
				t.Errorf("remove summary callback: %v", err)
			}
		}()

		result, callErr := editor.EditDraftWithVerification(ctx, change)
		if !injected {
			t.Fatal("summary failure injection did not execute")
		}
		if result != nil || !errors.Is(callErr, failure) {
			t.Fatalf("expected summary failure: result=%+v err=%v",
				result, callErr)
		}
		Equal(t, readVerificationState(t, observer, version.ID), before)
		Equal(t, Counts(t, observer), countsBefore)
	})

	if t.Failed() {
		t.Fatal("rollback case failed; stop subsequent writes")
	}

	// A提交修订2之后，B提交修订3并重新核验；
	// A的响应仍必须属于修订2，不能重新查询并混入B的结果。
	hookCalled := false
	var thirdVerification *model.RuleVerification
	store := &knowledgeAfterCommitStore{
		KnowledgeRepository: repository.NewKnowledgeRepository(editorDB),
	}
	store.after = func() error {
		hookCalled = true
		next := change
		next.ExpectedContentRevision = 2
		next.Content = "【虚构测试】第三次内容"
		next.Articles = []service.CreateRuleArticleInput{
			{
				SectionPath: "测试章节", ArticleNumber: "1",
				Content: "【虚构测试】第三次条款",
			},
		}
		if _, err := writer.EditDraftWithVerification(ctx, next); err != nil {
			return err
		}

		var err error
		thirdVerification, err = writer.VerifyDraft(
			ctx, userID, service.VerifyKnowledgeDraftInput{
				VersionID: version.ID, ExpectedContentRevision: 3,
				VerificationNote: "【虚构测试】第三次内容核验",
			},
		)
		return err
	}

	serviceWithInterleaving := service.NewKnowledgeService(store)
	result, err := serviceWithInterleaving.EditDraftWithVerification(ctx, change)
	Must(t, err)
	if !hookCalled || thirdVerification == nil {
		t.Fatal("after-commit interleaving did not execute")
	}
	if result == nil || result.Version == nil || result.Verification == nil {
		t.Fatal("edit result lost version or historical verification")
	}

	Equal(t, result.Version.ContentRevision, uint64(2))
	Equal(t, result.Version.Content, "【虚构测试】第二次内容")
	Equal(t, result.Verification.ID, firstVerification.ID)
	Equal(t, result.Verification.VerifiedContentRevision, uint64(1))

	current, err := writer.GetDraftDetailByID(ctx, version.ID)
	Must(t, err)
	Equal(t, current.Version.ContentRevision, uint64(3))
	Equal(t, current.Version.Content, "【虚构测试】第三次内容")
	if current.Verification == nil {
		t.Fatal("current verification missing")
	}
	Equal(t, current.Verification.ID, thirdVerification.ID)
	Equal(t, current.Verification.VerifiedContentRevision, uint64(3))
	Equal(t, len(current.Articles), 1)
	Equal(t, current.Articles[0].Content, "【虚构测试】第三次条款")

	t.Log("summary read failure rolled back the whole edit; " +
		"revision 2 response did not absorb later revision 3 or verification")
}

func TestKnowledgeRemainingMySQLLegacyMigration(t *testing.T) {
	requireKnowledgeRemainingMySQL(t)

	cfg := config.Load()
	base := OpenTestDB(t, cfg.Database)
	originalCounts := Counts(t, base)

	// 前缀仅由随机十六进制生成，不接受外部输入。
	var random [6]byte
	_, err := rand.Read(random[:])
	Must(t, err)
	prefix := "km_" + hex.EncodeToString(random[:]) + "_"

	// 顺序同时作为删除夹具表的外键依赖顺序。
	tableNames := []string{
		"rule_operations",
		"rule_verifications",
		"knowledge_chunks",
		"rule_articles",
		"rule_versions",
		"knowledge_documents",
		"rule_categories",
		"messages",
		"conversations",
		"users",
	}
	models := []any{
		&model.RuleOperation{},
		&model.RuleVerification{},
		&model.KnowledgeChunk{},
		&model.RuleArticle{},
		&model.RuleVersion{},
		&model.KnowledgeDocument{},
		&model.RuleCategory{},
		&model.Message{},
		&model.Conversation{},
		&model.User{},
	}

	// 先确认全部名称未被占用，之后才允许创建或清理这些表。
	for _, name := range tableNames {
		var count int64
		Must(t, base.Raw(`
			SELECT COUNT(*)
			FROM information_schema.TABLES
			WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?
		`, prefix+name).Scan(&count).Error)
		if count != 0 {
			t.Fatalf("migration fixture name already exists: %s", prefix+name)
		}
	}

	pool, err := base.DB()
	Must(t, err)

	// 使用新的GORM实例和Schema缓存，避免沿用原表名缓存。
	// 连接仍由OpenTestDB管理，不重复关闭底层连接池。
	isolated, err := gorm.Open(
		mysqlDialect.New(mysqlDialect.Config{Conn: pool}),
		&gorm.Config{
			NamingStrategy: schema.NamingStrategy{TablePrefix: prefix},
			Logger:         base.Logger,
		},
	)
	Must(t, err)

	// 在任何DDL之前确认全部模型确实映射到随机前缀表。
	for i, value := range models {
		statement := &gorm.Statement{DB: isolated}
		Must(t, statement.Parse(value))
		Equal(t, statement.Schema.Table, prefix+tableNames[i])
	}

	t.Cleanup(func() {
		// 只删除本次保留名称下的夹具表，不关闭外键检查。
		// 某张表删除失败立即停止，保留现场，不扩大清理范围。
		for _, name := range tableNames {
			query := "DROP TABLE IF EXISTS `" + prefix + name + "`"
			if err := base.Exec(query).Error; err != nil {
				t.Errorf("drop owned migration fixture %s: %v",
					prefix+name, err)
				return
			}
		}
		Equal(t, Counts(t, base), originalCounts)
	})

	Must(t, database.Migrate(
		isolated.Set("gorm:table_options", "ENGINE=InnoDB"),
	))

	// 确认夹具外键没有引用已有表或其他数据库。
	var foreignKeys []struct {
		ParentSchema string
		ParentTable  string
	}
	ownedNames := make([]string, len(tableNames))
	owned := make(map[string]bool, len(tableNames))
	for i, name := range tableNames {
		ownedNames[i] = prefix + name
		owned[prefix+name] = true
	}
	Must(t, base.Raw(`
		SELECT REFERENCED_TABLE_SCHEMA AS parent_schema,
		       REFERENCED_TABLE_NAME AS parent_table
		FROM information_schema.KEY_COLUMN_USAGE
		WHERE TABLE_SCHEMA = DATABASE()
		  AND TABLE_NAME IN ?
		  AND REFERENCED_TABLE_NAME IS NOT NULL
	`, ownedNames).Scan(&foreignKeys).Error)
	if len(foreignKeys) == 0 {
		t.Fatal("migration fixture unexpectedly has no foreign keys")
	}
	for _, fk := range foreignKeys {
		if fk.ParentSchema != "bili_support_test" || !owned[fk.ParentTable] {
			t.Fatalf("fixture FK escapes isolated tables: %+v", fk)
		}
	}

	// 仅将空的夹具表降至阶段02之前的结构。
	// 原有users/rule_versions等无前缀表不会被修改。
	Must(t, isolated.Exec(
		"DROP TABLE `"+prefix+"rule_verifications`",
	).Error)
	Must(t, isolated.Exec(
		"ALTER TABLE `"+prefix+
			"rule_versions` DROP COLUMN `content_revision`",
	).Error)

	user := model.User{
		Username: "legacy-fixture", PasswordHash: "not-a-login-password",
		Role: model.RoleAdmin,
	}
	Must(t, isolated.Omit(clause.Associations).Create(&user).Error)

	conversation := model.Conversation{
		UserID: user.ID, Title: "【虚构测试】迁移保留会话",
	}
	Must(t, isolated.Omit(clause.Associations).Create(&conversation).Error)
	message := model.Message{
		ConversationID: conversation.ID,
		Role:           "user",
		Content:        "【虚构测试】迁移保留消息",
	}
	Must(t, isolated.Omit(clause.Associations).Create(&message).Error)

	category := model.RuleCategory{
		Code: "legacy-fixture", Name: "【虚构测试】旧分类",
	}
	Must(t, isolated.Omit(clause.Associations).Create(&category).Error)
	document := model.KnowledgeDocument{
		DocumentKey: "legacy-fixture",
	}
	Must(t, isolated.Omit(clause.Associations).Create(&document).Error)

	version := model.RuleVersion{
		DocumentID: document.ID, Version: "legacy-v1", CategoryID: category.ID,
		Title:     "【虚构测试】迁移前草稿",
		Content:   "【虚构测试】迁移前原文\n不得丢失",
		SourceURL: "https://example.invalid/legacy-fixture",
		Status:    model.RuleVersionStatusDraft,
	}
	// 旧表还没有该列，插入时明确忽略，不让GORM尝试写入默认值。
	Must(t, isolated.Omit(clause.Associations, "ContentRevision").
		Create(&version).Error)

	article := model.RuleArticle{
		VersionID: version.ID, ArticleOrder: 1,
		SectionPath: "旧章节", ArticleNumber: "1",
		Content: "【虚构测试】旧条款",
	}
	Must(t, isolated.Omit(clause.Associations).Create(&article).Error)
	chunk := model.KnowledgeChunk{
		ArticleID: article.ID, ChunkIndex: 0, Content: article.Content,
	}
	Must(t, isolated.Omit(clause.Associations).Create(&chunk).Error)

	// 完整记录八张旧表的行内容，不仅比较COUNT。
	// content_revision是本次新增列，另行精确断言。
	readOldData := func() map[string][]map[string]any {
		t.Helper()
		data := make(map[string][]map[string]any)
		for _, name := range tableNames {
			if name == "rule_verifications" || name == "rule_operations" {
				continue
			}
			var rows []map[string]any
			Must(t, isolated.Table(prefix+name).
				Order("id ASC").Find(&rows).Error)
			if name == "rule_versions" {
				for _, row := range rows {
					delete(row, "content_revision")
				}
			}
			data[name] = rows
		}
		return data
	}
	before := readOldData()
	for name, rows := range before {
		if len(rows) != 1 {
			t.Fatalf("expected nonempty legacy fixture in %s, got %d rows",
				name, len(rows))
		}
	}

	assertMigrated := func() {
		t.Helper()
		Equal(t, readOldData(), before)

		var saved model.RuleVersion
		Must(t, isolated.First(&saved, version.ID).Error)
		Equal(t, saved.ContentRevision, uint64(1))
		Equal(t, saved.Status, model.RuleVersionStatusDraft)

		var verificationCount int64
		Must(t, isolated.Model(&model.RuleVerification{}).
			Count(&verificationCount).Error)
		Equal(t, verificationCount, int64(0))

		var operationCount int64
		Must(t, isolated.Model(&model.RuleOperation{}).
			Count(&operationCount).Error)
		Equal(t, operationCount, int64(0))

		var revisionColumns int64
		Must(t, isolated.Raw(`
			SELECT COUNT(*)
			FROM information_schema.COLUMNS
			WHERE TABLE_SCHEMA = DATABASE()
			  AND TABLE_NAME = ?
			  AND COLUMN_NAME = 'content_revision'
			  AND DATA_TYPE = 'bigint'
			  AND COLUMN_TYPE LIKE '%unsigned%'
			  AND IS_NULLABLE = 'NO'
			  AND COLUMN_DEFAULT = '1'
		`, prefix+"rule_versions").Scan(&revisionColumns).Error)
		Equal(t, revisionColumns, int64(1))
	}

	// 调用项目真实迁移入口，而不是在测试里另写升级SQL。
	Must(t, database.Migrate(isolated))
	assertMigrated()

	// 重复迁移同样不能改写旧内容或自动产生核验。
	Must(t, database.Migrate(isolated))
	assertMigrated()

	Equal(t, Counts(t, base), originalCounts)
	t.Logf("migration prefix=%s: legacy rows preserved; "+
		"revision default=1; verification count=0; repeated migration preserved data",
		prefix)
}
