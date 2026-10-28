package router

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"video-support-agent/internal/config"
	"video-support-agent/internal/model"
	"video-support-agent/internal/repository"
	"video-support-agent/internal/service"

	mysqlDriver "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func TestKnowledgeVerificationMySQLConcurrency(t *testing.T) {
	if os.Getenv("RUN_MYSQL_TESTS") != "1" ||
		os.Getenv("DB_NAME") != "bili_support_test" ||
		os.Getenv("TEST_ISOLATED") != "1" {
		t.Fatal("require RUN_MYSQL_TESTS=1, DB_NAME=bili_support_test, TEST_ISOLATED=1")
	}

	cases := []struct {
		name   string
		first  string
		second string
	}{
		{"edit_then_verify", "edit", "verify"},
		{"verify_then_edit", "verify", "edit"},
		{"edit_then_edit", "edit", "edit"},
	}

	for _, tc := range cases {
		if !t.Run(tc.name, func(t *testing.T) {
			runKnowledgeCompetition(t, tc.first, tc.second)
		}) {
			t.Fatal("competition failed; stop remaining cases")
		}
	}
}

func runKnowledgeCompetition(t *testing.T, firstOp, secondOp string) {
	t.Helper()
	cfg := config.Load()

	firstDB := OpenTestDB(t, cfg.Database)
	secondDB := OpenTestDB(t, cfg.Database)
	observer := OpenTestDB(t, cfg.Database)

	// 各连接池只保留一个连接，使连接身份检查对应后续操作。
	var connectionIDs []uint64
	for _, db := range []*gorm.DB{firstDB, secondDB, observer} {
		pool, err := db.DB()
		Must(t, err)
		pool.SetMaxOpenConns(1)
		pool.SetMaxIdleConns(1)

		var id uint64
		Must(t, db.Raw("SELECT CONNECTION_ID()").Scan(&id).Error)
		connectionIDs = append(connectionIDs, id)
	}
	if connectionIDs[0] == connectionIDs[1] ||
		connectionIDs[0] == connectionIDs[2] ||
		connectionIDs[1] == connectionIDs[2] {
		t.Fatalf("expected three independent connections: %v", connectionIDs)
	}
	t.Logf("independent MySQL connections: %v", connectionIDs)

	initial := Counts(t, observer)
	var random [12]byte
	_, err := rand.Read(random[:])
	Must(t, err)
	key := "test-race-" + hex.EncodeToString(random[:])

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

	category := model.RuleCategory{Code: key, Name: "虚构并发测试分类"}
	Must(t, observer.Create(&category).Error)
	categoryID = category.ID
	user := model.User{
		Username: key, PasswordHash: "not-a-login-password",
		Role: model.RoleAdmin,
	}
	Must(t, observer.Create(&user).Error)
	userID = user.ID

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	setup := service.NewKnowledgeService(repository.NewKnowledgeRepository(observer))
	version, err := setup.CreateDraft(ctx, service.CreateKnowledgeDraftInput{
		DocumentKey: key, Version: "test-v1", CategoryID: categoryID,
		Title: "虚构事务测试", Content: "虚构初始内容",
		SourceURL: "https://www.bilibili.com/test-only",
		Articles: []service.CreateRuleArticleInput{
			{SectionPath: "虚构章节", ArticleNumber: "1", Content: "初始条款"},
		},
	})
	Must(t, err)

	firstLocked := make(chan struct{})
	secondEntering := make(chan struct{})
	releaseFirst := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseFirst) }) }

	var workers sync.WaitGroup
	// 即使断言失败，也先释放/取消并等待工作协程，再清理数据库。
	defer func() {
		release()
		cancel()
		workers.Wait()
	}()

	var firstOnce, secondOnce sync.Once
	firstCallback := key + "-first-lock"
	secondCallback := key + "-second-query"

	Must(t, firstDB.Callback().Query().After("gorm:query").
		Register(firstCallback, func(tx *gorm.DB) {
			if tx.Error != nil || tx.Statement.Schema == nil ||
				tx.Statement.Schema.Table != "rule_versions" {
				return
			}
			if _, exists := tx.Statement.Clauses["FOR"]; !exists {
				return
			}
			firstOnce.Do(func() {
				// 查询已经返回，事务仍未结束，实际锁由下方NOWAIT探针确认。
				close(firstLocked)
				select {
				case <-releaseFirst:
				case <-ctx.Done():
					tx.AddError(ctx.Err())
				}
			})
		}))
	t.Cleanup(func() {
		if err := firstDB.Callback().Query().Remove(firstCallback); err != nil {
			t.Errorf("remove first callback: %v", err)
		}
	})

	Must(t, secondDB.Callback().Query().Before("gorm:query").
		Register(secondCallback, func(tx *gorm.DB) {
			if tx.Statement.Schema == nil ||
				tx.Statement.Schema.Table != "rule_versions" {
				return
			}
			if _, exists := tx.Statement.Clauses["FOR"]; !exists {
				return
			}
			secondOnce.Do(func() { close(secondEntering) })
		}))
	t.Cleanup(func() {
		if err := secondDB.Callback().Query().Remove(secondCallback); err != nil {
			t.Errorf("remove second callback: %v", err)
		}
	})

	type outcome struct {
		edit   *service.KnowledgeDraftEditResult
		verify *model.RuleVerification
		err    error
	}
	start := func(db *gorm.DB, operation, label string) <-chan outcome {
		done := make(chan outcome, 1)
		workers.Add(1)
		go func() {
			defer workers.Done()
			svc := service.NewKnowledgeService(repository.NewKnowledgeRepository(db))
			var result outcome
			switch operation {
			case "edit":
				result.edit, result.err = svc.EditDraftWithVerification(
					ctx, service.EditKnowledgeDraftInput{
						VersionID: version.ID, ExpectedContentRevision: 1,
						CategoryID: categoryID, Title: "虚构事务测试",
						Content:   "虚构编辑-" + label,
						SourceURL: "https://www.bilibili.com/test-only",
						Articles: []service.CreateRuleArticleInput{
							{
								SectionPath: "虚构章节", ArticleNumber: "1",
								Content: "虚构条款-" + label,
							},
						},
					},
				)
			case "verify":
				result.verify, result.err = svc.VerifyDraft(
					ctx, userID, service.VerifyKnowledgeDraftInput{
						VersionID: version.ID, ExpectedContentRevision: 1,
						VerificationNote: "【虚构测试】并发核验",
					},
				)
			default:
				result.err = fmt.Errorf("unknown test operation %q", operation)
			}
			done <- result
		}()
		return done
	}

	awaitSignal := func(ch <-chan struct{}, name string) {
		t.Helper()
		select {
		case <-ch:
		case <-ctx.Done():
			t.Fatalf("waiting for %s: %v", name, ctx.Err())
		}
	}
	awaitResult := func(ch <-chan outcome, name string) outcome {
		t.Helper()
		select {
		case result := <-ch:
			return result
		case <-ctx.Done():
			t.Fatalf("waiting for %s result: %v", name, ctx.Err())
			return outcome{}
		}
	}

	firstDone := start(firstDB, firstOp, "first")
	awaitSignal(firstLocked, "first version lock")

	// 独立事务尝试立即取得同一版本锁，必须被MySQL拒绝。
	// 不写数据；无论成功或失败，Transaction都负责结束该事务。
	probeErr := observer.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var locked model.RuleVersion
		return tx.Clauses(clause.Locking{
			Strength: "UPDATE",
			Options:  "NOWAIT",
		}).Where("id = ?", version.ID).First(&locked).Error
	})
	var mysqlErr *mysqlDriver.MySQLError
	if !errors.As(probeErr, &mysqlErr) || mysqlErr.Number != 3572 {
		t.Fatalf("expected NOWAIT lock conflict 3572, got %v", probeErr)
	}
	t.Log("NOWAIT probe confirmed the first transaction holds the version lock")

	secondDone := start(secondDB, secondOp, "second")
	awaitSignal(secondEntering, "second version query")
	release()

	first := awaitResult(firstDone, "first")
	second := awaitResult(secondDone, "second")
	workers.Wait()

	if first.err != nil {
		t.Fatalf("first %s failed: %v", firstOp, first.err)
	}
	if firstOp == "edit" {
		if first.edit == nil || first.edit.Version.ContentRevision != 2 {
			t.Fatalf("wrong first edit result: %+v", first.edit)
		}
		if !errors.Is(second.err, service.ErrKnowledgeContentConflict) {
			t.Fatalf("expected second operation conflict, got %v", second.err)
		}
		if second.edit != nil || second.verify != nil {
			t.Fatal("conflicting operation returned a success result")
		}
	} else {
		if first.verify == nil || first.verify.VerifiedContentRevision != 1 {
			t.Fatalf("wrong first verification result: %+v", first.verify)
		}
		if second.err != nil || second.edit == nil ||
			second.edit.Version.ContentRevision != 2 {
			t.Fatalf("second edit: result=%+v err=%v", second.edit, second.err)
		}
	}

	state := readVerificationState(t, observer, version.ID)
	Equal(t, state.Version.ContentRevision, uint64(2))
	Equal(t, state.Document.PublicationRevision, uint64(0))
	Equal(t, len(state.Articles), 1)
	Equal(t, len(state.Chunks), 1)
	Equal(t, state.Articles[0].VersionID, version.ID)
	Equal(t, state.Chunks[0].ArticleID, state.Articles[0].ID)
	Equal(t, state.Chunks[0].ChunkIndex, uint32(0))

	winner := "first"
	if firstOp == "verify" {
		winner = "second"
	}
	Equal(t, state.Version.Content, "虚构编辑-"+winner)
	Equal(t, state.Articles[0].Content, "虚构条款-"+winner)
	Equal(t, state.Chunks[0].Content, "虚构条款-"+winner)

	detail, err := setup.GetDraftDetailByID(ctx, version.ID)
	Must(t, err)
	if firstOp == "verify" {
		Equal(t, len(state.Verifications), 1)
		Equal(t, state.Verifications[0].VerifiedBy, userID)
		assertVerificationSnapshot(
			t, state.Verifications[0], version.ID, 1,
			"虚构初始内容", []string{"初始条款"},
		)
		if detail.Verification == nil {
			t.Fatal("historical verification disappeared")
		}
		Equal(t, detail.Verification.ID, first.verify.ID)
		Equal(t, detail.Verification.VerifiedContentRevision, uint64(1))
		Equal(t, detail.Version.ContentRevision, uint64(2))
	} else {
		Equal(t, len(state.Verifications), 0)
		if detail.Verification != nil {
			t.Fatal("conflicting verification unexpectedly persisted")
		}
	}
}

func TestKnowledgeVerificationMySQLDetailSnapshot(t *testing.T) {
	if os.Getenv("RUN_MYSQL_TESTS") != "1" ||
		os.Getenv("DB_NAME") != "bili_support_test" ||
		os.Getenv("TEST_ISOLATED") != "1" {
		t.Fatal("require RUN_MYSQL_TESTS=1, DB_NAME=bili_support_test, TEST_ISOLATED=1")
	}

	cfg := config.Load()
	readerDB := OpenTestDB(t, cfg.Database)
	writerDB := OpenTestDB(t, cfg.Database)

	// 两个独立连接，避免误把串行调用当成并发场景。
	var connectionIDs []uint64
	for _, db := range []*gorm.DB{readerDB, writerDB} {
		pool, err := db.DB()
		Must(t, err)
		pool.SetMaxOpenConns(1)
		pool.SetMaxIdleConns(1)

		var id uint64
		Must(t, db.Raw("SELECT CONNECTION_ID()").Scan(&id).Error)
		connectionIDs = append(connectionIDs, id)
	}
	if connectionIDs[0] == connectionIDs[1] {
		t.Fatalf("expected independent connections: %v", connectionIDs)
	}
	t.Logf("reader/writer connections: %v", connectionIDs)

	initial := Counts(t, writerDB)
	var random [12]byte
	_, err := rand.Read(random[:])
	Must(t, err)
	key := "test-detail-snapshot-" + hex.EncodeToString(random[:])

	var userID, categoryID uint64
	t.Cleanup(func() {
		var users, categories []uint64
		if userID != 0 {
			users = []uint64{userID}
		}
		if categoryID != 0 {
			categories = []uint64{categoryID}
		}
		if err := Cleanup(writerDB, []string{key}, users, categories); err != nil {
			t.Errorf("cleanup failed: %v; do not delete unrelated data", err)
			return
		}
		Equal(t, Counts(t, writerDB), initial)
	})

	category := model.RuleCategory{
		Code: key,
		Name: "【虚构测试】详情快照分类",
	}
	Must(t, writerDB.Create(&category).Error)
	categoryID = category.ID

	user := model.User{
		Username:     key,
		PasswordHash: "test-fixture-not-a-login-password",
		Role:         model.RoleAdmin,
	}
	Must(t, writerDB.Create(&user).Error)
	userID = user.ID

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	writer := service.NewKnowledgeService(
		repository.NewKnowledgeRepository(writerDB),
	)
	reader := service.NewKnowledgeService(
		repository.NewKnowledgeRepository(readerDB),
	)

	version, err := writer.CreateDraft(ctx, service.CreateKnowledgeDraftInput{
		DocumentKey: key,
		Version:     "test-v1",
		CategoryID:  categoryID,
		Title:       "【虚构测试】旧标题",
		Content:     "【虚构测试】旧正文",
		SourceURL:   "https://www.bilibili.com/test-only",
		Articles: []service.CreateRuleArticleInput{
			{
				SectionPath:   "旧章节",
				ArticleNumber: "1",
				Content:       "【虚构测试】旧条款",
			},
		},
	})
	Must(t, err)

	firstVerification, err := writer.VerifyDraft(
		ctx, userID, service.VerifyKnowledgeDraftInput{
			VersionID:               version.ID,
			ExpectedContentRevision: 1,
			VerificationNote:        "【虚构测试】旧内容核验",
		},
	)
	Must(t, err)

	oldDetail, err := writer.GetDraftDetailByID(ctx, version.ID)
	Must(t, err)
	if oldDetail.Verification == nil {
		t.Fatal("initial verification summary missing")
	}
	Equal(t, oldDetail.Version.ContentRevision, uint64(1))
	Equal(t, oldDetail.Verification.ID, firstVerification.ID)

	versionRead := make(chan struct{})
	resumeRead := make(chan struct{})
	var pauseOnce, releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() { close(resumeRead) })
	}

	var workers sync.WaitGroup
	// 任何断言失败时，也先结束读取协程，再清理数据库。
	defer func() {
		release()
		cancel()
		workers.Wait()
	}()

	// 只在本测试reader连接上暂停；不修改生产代码。
	callbackName := key + "-pause-after-version"
	Must(t, readerDB.Callback().Query().After("gorm:query").
		Register(callbackName, func(tx *gorm.DB) {
			if tx.Error != nil || tx.Statement.Schema == nil ||
				tx.Statement.Schema.Table != "rule_versions" {
				return
			}
			pauseOnce.Do(func() {
				close(versionRead)
				select {
				case <-resumeRead:
				case <-ctx.Done():
					tx.AddError(ctx.Err())
				}
			})
		}))
	t.Cleanup(func() {
		if err := readerDB.Callback().Query().Remove(callbackName); err != nil {
			t.Errorf("remove snapshot callback: %v", err)
		}
	})

	type outcome struct {
		detail *service.KnowledgeDraftDetail
		err    error
	}
	done := make(chan outcome, 1)

	workers.Add(1)
	go func() {
		defer workers.Done()
		detail, callErr := reader.GetDraftDetailByID(ctx, version.ID)
		done <- outcome{detail: detail, err: callErr}
	}()

	select {
	case <-versionRead:
	case <-ctx.Done():
		t.Fatalf("reader did not reach version query: %v", ctx.Err())
	}

	// A已经读到旧版本，但还没开始读取条款与核验。
	// B必须在A恢复之前完成编辑和重新核验。
	editResult, err := writer.EditDraftWithVerification(
		ctx, service.EditKnowledgeDraftInput{
			VersionID:               version.ID,
			ExpectedContentRevision: 1,
			CategoryID:              categoryID,
			Title:                   "【虚构测试】新标题",
			Content:                 "【虚构测试】新正文",
			SourceURL:               "https://www.bilibili.com/test-only",
			Articles: []service.CreateRuleArticleInput{
				{
					SectionPath:   "新章节",
					ArticleNumber: "1",
					Content:       "【虚构测试】新条款一",
				},
				{
					SectionPath:   "新章节",
					ArticleNumber: "2",
					Content:       "【虚构测试】新条款二",
				},
			},
		},
	)
	Must(t, err)
	Equal(t, editResult.Version.ContentRevision, uint64(2))

	secondVerification, err := writer.VerifyDraft(
		ctx, userID, service.VerifyKnowledgeDraftInput{
			VersionID:               version.ID,
			ExpectedContentRevision: 2,
			VerificationNote:        "【虚构测试】新内容核验",
		},
	)
	Must(t, err)
	if secondVerification.ID <= firstVerification.ID {
		t.Fatal("new verification was not appended")
	}

	// 新请求已经能看到新状态，证明写事务确实提交了。
	newDetail, err := writer.GetDraftDetailByID(ctx, version.ID)
	Must(t, err)
	Equal(t, newDetail.Version.ContentRevision, uint64(2))
	Equal(t, newDetail.Version.Title, "【虚构测试】新标题")
	Equal(t, newDetail.Version.Content, "【虚构测试】新正文")
	Equal(t, len(newDetail.Articles), 2)
	for i, article := range newDetail.Articles {
		Equal(t, article.VersionID, version.ID)
		Equal(t, article.ArticleOrder, uint32(i+1))
		Equal(t, article.SectionPath, "新章节")
	}
	Equal(t, newDetail.Articles[0].Content, "【虚构测试】新条款一")
	Equal(t, newDetail.Articles[1].Content, "【虚构测试】新条款二")
	if newDetail.Verification == nil {
		t.Fatal("new verification summary missing")
	}
	Equal(t, newDetail.Verification.ID, secondVerification.ID)
	Equal(t, newDetail.Verification.VerifiedContentRevision, uint64(2))

	release()

	var observed outcome
	select {
	case observed = <-done:
	case <-ctx.Done():
		t.Fatalf("reader did not complete: %v", ctx.Err())
	}
	workers.Wait()
	Must(t, observed.err)

	// 必须整体等于旧详情，不能只验证版本修订号。
	Equal(t, observed.detail, oldDetail)

	// 同一reader发起下一次请求，必须看到新状态。
	// 证明上一请求返回旧数据源于事务快照，而非一直返回旧内容。
	freshDetail, err := reader.GetDraftDetailByID(ctx, version.ID)
	Must(t, err)
	Equal(t, freshDetail, newDetail)

	t.Log("writer committed revision 2 and new verification while reader paused; " +
		"in-flight read retained the complete revision 1 detail; " +
		"next read returned the complete revision 2 detail")
}
