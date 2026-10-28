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

	"video-support-agent/internal/config"
	"video-support-agent/internal/model"
	"video-support-agent/internal/repository"
	"video-support-agent/internal/service"

	"gorm.io/gorm"
)

// 保存实际数据库状态，比较失败前后是否发生任何持久化变更。
// 只查询本测试文档的数据，不读取其他文档正文。
type verificationDBState struct {
	Document      model.KnowledgeDocument
	Version       model.RuleVersion
	Articles      []model.RuleArticle
	Chunks        []model.KnowledgeChunk
	Verifications []model.RuleVerification
}

func readVerificationState(
	t *testing.T, db *gorm.DB, versionID uint64,
) verificationDBState {
	t.Helper()
	var state verificationDBState
	Must(t, db.First(&state.Version, versionID).Error)
	Must(t, db.First(&state.Document, state.Version.DocumentID).Error)
	Must(t, db.Where("version_id = ?", versionID).
		Order("article_order ASC").Find(&state.Articles).Error)

	ids := make([]uint64, len(state.Articles))
	for i, article := range state.Articles {
		ids[i] = article.ID
	}
	if len(ids) > 0 {
		Must(t, db.Where("article_id IN ?", ids).
			Order("article_id ASC, chunk_index ASC").
			Find(&state.Chunks).Error)
	}
	Must(t, db.Where("version_id = ?", versionID).
		Order("id ASC").Find(&state.Verifications).Error)
	return state
}

func assertVerificationSnapshot(
	t *testing.T, record model.RuleVerification,
	versionID uint64, revision uint64, content string, texts []string,
) {
	t.Helper()
	var snapshot struct {
		VersionID       uint64     `json:"version_id"`
		Version         string     `json:"version"`
		ContentRevision uint64     `json:"content_revision"`
		CategoryID      uint64     `json:"category_id"`
		Title           string     `json:"title"`
		Content         string     `json:"content"`
		SourceURL       string     `json:"source_url"`
		EffectiveAt     *time.Time `json:"effective_at"`
		ExpiresAt       *time.Time `json:"expires_at"`
		Articles        []struct {
			Order   uint32 `json:"article_order"`
			Section string `json:"section_path"`
			Number  string `json:"article_number"`
			Content string `json:"content"`
		} `json:"articles"`
	}
	Must(t, json.Unmarshal([]byte(record.ContentSnapshot), &snapshot))
	Equal(t, record.SnapshotSchemaVersion, uint32(2))
	Equal(t, record.VersionID, versionID)
	Equal(t, record.VerifiedContentRevision, revision)
	Equal(t, snapshot.VersionID, versionID)
	Equal(t, snapshot.Version, "test-v1")
	Equal(t, snapshot.ContentRevision, revision)
	Equal(t, snapshot.Content, content)
	Equal(t, snapshot.Title, "虚构事务测试")
	Equal(t, snapshot.SourceURL, "https://www.bilibili.com/test-only")
	Equal(t, snapshot.EffectiveAt, (*time.Time)(nil))
	Equal(t, snapshot.ExpiresAt, (*time.Time)(nil))
	if snapshot.CategoryID == 0 {
		t.Fatal("snapshot lost category")
	}
	Equal(t, len(snapshot.Articles), len(texts))
	for i, article := range snapshot.Articles {
		Equal(t, article.Order, uint32(i+1))
		Equal(t, article.Section, "虚构章节")
		Equal(t, article.Number, fmt.Sprintf("%d", i+1))
		Equal(t, article.Content, texts[i])
	}
}

func TestKnowledgeVerificationMySQLTransactions(t *testing.T) {
	if os.Getenv("RUN_MYSQL_TESTS") != "1" {
		t.Fatal("explicit RUN_MYSQL_TESTS=1 required")
	}
	if os.Getenv("DB_NAME") != "bili_support_test" {
		t.Fatal("explicit DB_NAME=bili_support_test required")
	}
	if os.Getenv("TEST_ISOLATED") != "1" {
		t.Fatal("stop other writers, then explicitly set TEST_ISOLATED=1")
	}

	cfg := config.Load()
	db := OpenTestDB(t, cfg.Database)
	observer := OpenTestDB(t, cfg.Database)
	initial := Counts(t, observer)

	var random [12]byte
	_, err := rand.Read(random[:])
	Must(t, err)
	key := "test-verification-" + hex.EncodeToString(random[:])

	var userID, categoryID, boundaryCategoryID uint64
	t.Cleanup(func() {
		var users, categories []uint64
		if userID != 0 {
			users = []uint64{userID}
		}
		if categoryID != 0 {
			categories = []uint64{categoryID}
		}
		if boundaryCategoryID != 0 {
			categories = append(categories, boundaryCategoryID)
		}
		if err := Cleanup(observer, []string{key, key + "-boundary"}, users, categories); err != nil {
			t.Errorf("fixture cleanup failed: %v; do not delete unrelated data", err)
			return
		}
		Equal(t, Counts(t, observer), initial)
	})

	category := model.RuleCategory{Code: key, Name: "虚构测试分类"}
	Must(t, db.Create(&category).Error)
	categoryID = category.ID

	user := model.User{
		Username: key, PasswordHash: "not-a-login-password",
		Role: model.RoleAdmin,
	}
	Must(t, db.Create(&user).Error)
	userID = user.ID

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	svc := service.NewKnowledgeService(repository.NewKnowledgeRepository(db))

	input := service.CreateKnowledgeDraftInput{
		DocumentKey: key, Version: "test-v1", CategoryID: categoryID,
		Title: "虚构事务测试", Content: "虚构原文\n甲\n乙",
		SourceURL: "https://www.bilibili.com/test-only",
		Articles: []service.CreateRuleArticleInput{
			{SectionPath: "虚构章节", ArticleNumber: "1", Content: "甲"},
			{SectionPath: "虚构章节", ArticleNumber: "2", Content: "乙"},
		},
	}
	version, err := svc.CreateDraft(ctx, input)
	Must(t, err)
	Equal(t, version.ContentRevision, uint64(1))

	detail, err := svc.GetDraftDetailByID(ctx, version.ID)
	Must(t, err)
	if detail.Verification != nil {
		t.Fatal("new draft was automatically verified")
	}

	evidence := "https://www.bilibili.com/test-evidence-only"
	verifyInput := service.VerifyKnowledgeDraftInput{
		VersionID: version.ID, ExpectedContentRevision: 1,
		VerificationNote: "【虚构测试】首次核验", EvidenceURL: &evidence,
	}

	started := time.Now().UTC().Truncate(time.Millisecond)
	first, err := svc.VerifyDraft(ctx, userID, verifyInput)
	Must(t, err)
	if first.ID == 0 || first.VerifiedAt.Before(started) ||
		first.VerifiedAt.After(time.Now().UTC()) {
		t.Fatalf("invalid verification ID or server time: %+v", first)
	}

	initialVerified := readVerificationState(t, observer, version.ID)
	Equal(t, initialVerified.Version.ContentRevision, uint64(1))
	Equal(t, len(initialVerified.Verifications), 1)
	savedFirst := initialVerified.Verifications[0]
	Equal(t, savedFirst.ID, first.ID)
	Equal(t, savedFirst.VerifiedBy, userID)
	Equal(t, savedFirst.VerificationNote, "【虚构测试】首次核验")
	Equal(t, savedFirst.EvidenceURL, &evidence)
	Equal(t, savedFirst.VerifiedAt.Equal(first.VerifiedAt), true)
	assertVerificationSnapshot(t, savedFirst, version.ID, 1,
		"虚构原文\n甲\n乙", []string{"甲", "乙"})

	edit := service.EditKnowledgeDraftInput{
		VersionID: version.ID, ExpectedContentRevision: 1,
		CategoryID: categoryID, Title: "虚构事务测试",
		Content:   "虚构新原文\n丙\n丁",
		SourceURL: "https://www.bilibili.com/test-only",
		Articles: []service.CreateRuleArticleInput{
			{SectionPath: "虚构章节", ArticleNumber: "1", Content: "丙"},
			{SectionPath: "虚构章节", ArticleNumber: "2", Content: "丁"},
		},
	}
	edited, err := svc.EditDraftWithVerification(ctx, edit)
	Must(t, err)
	Equal(t, edited.Version.ContentRevision, uint64(2))
	if edited.Verification == nil {
		t.Fatal("edit lost historical verification summary")
	}
	Equal(t, edited.Verification.ID, first.ID)
	Equal(t, edited.Verification.VerifiedContentRevision, uint64(1))

	afterEdit := readVerificationState(t, observer, version.ID)
	Equal(t, afterEdit.Version.Content, edit.Content)
	Equal(t, afterEdit.Document.PublicationRevision,
		initialVerified.Document.PublicationRevision)
	Equal(t, afterEdit.Verifications, initialVerified.Verifications)
	Equal(t, len(afterEdit.Articles), 2)
	Equal(t, len(afterEdit.Chunks), 2)

	oldArticleIDs := map[uint64]bool{}
	for _, article := range initialVerified.Articles {
		oldArticleIDs[article.ID] = true
	}
	for i, article := range afterEdit.Articles {
		if oldArticleIDs[article.ID] {
			t.Fatal("edit unexpectedly reused an old article ID")
		}
		Equal(t, article.VersionID, version.ID)
		Equal(t, article.ArticleOrder, uint32(i+1))
		Equal(t, article.Content, edit.Articles[i].Content)
		var chunks []model.KnowledgeChunk
		Must(t, observer.Where("article_id = ?", article.ID).Find(&chunks).Error)
		Equal(t, len(chunks), 1)
		Equal(t, chunks[0].ChunkIndex, uint32(0))
		Equal(t, chunks[0].Content, article.Content)
	}
	for id := range oldArticleIDs {
		var count int64
		Must(t, observer.Model(&model.KnowledgeChunk{}).
			Where("article_id = ?", id).Count(&count).Error)
		Equal(t, count, int64(0))
	}

	// 旧修订号编辑/核验必须拒绝，不能留下任何持久化修改。
	gotEdit, err := svc.EditDraftWithVerification(ctx, edit)
	if gotEdit != nil || !errors.Is(err, service.ErrKnowledgeContentConflict) {
		t.Fatalf("stale edit: result=%+v err=%v", gotEdit, err)
	}
	gotVerify, err := svc.VerifyDraft(ctx, userID, verifyInput)
	if gotVerify != nil || !errors.Is(err, service.ErrKnowledgeContentConflict) {
		t.Fatalf("stale verification: result=%+v err=%v", gotVerify, err)
	}
	Equal(t, readVerificationState(t, observer, version.ID), afterEdit)

	// 重新核验与重复核验都追加记录，不覆盖旧快照。
	verifyInput.ExpectedContentRevision = 2
	verifyInput.VerificationNote = "【虚构测试】重新核验"
	second, err := svc.VerifyDraft(ctx, userID, verifyInput)
	Must(t, err)
	third, err := svc.VerifyDraft(ctx, userID, verifyInput)
	Must(t, err)
	if second.ID <= first.ID || third.ID <= second.ID {
		t.Fatal("verification records were not appended")
	}

	verifiedAgain := readVerificationState(t, observer, version.ID)
	Equal(t, len(verifiedAgain.Verifications), 3)
	Equal(t, verifiedAgain.Verifications[0], savedFirst)
	assertVerificationSnapshot(t, verifiedAgain.Verifications[1],
		version.ID, 2, edit.Content, []string{"丙", "丁"})
	assertVerificationSnapshot(t, verifiedAgain.Verifications[2],
		version.ID, 2, edit.Content, []string{"丙", "丁"})

	detail, err = svc.GetDraftDetailByID(ctx, version.ID)
	Must(t, err)
	if detail.Verification == nil {
		t.Fatal("latest verification missing")
	}
	Equal(t, detail.Verification.ID, third.ID)
	Equal(t, detail.Verification.VerifiedContentRevision,
		detail.Version.ContentRevision)

	// 失败注入只存在于本测试GORM连接，不修改生产代码或数据库结构。
	edit.ExpectedContentRevision = 2
	edit.Content = "不应提交的虚构内容"
	edit.Articles[0].Content = "不应提交的虚构条款"

	for _, table := range []string{
		"rule_articles", "knowledge_chunks", "rule_verifications",
	} {
		ok := t.Run("rollback_"+table, func(t *testing.T) {
			before := readVerificationState(t, observer, version.ID)
			countsBefore := Counts(t, observer)
			failure := errors.New("controlled transaction failure")
			callbackName := key + "-" + table
			seen, injected := 0, false
			target := 2
			if table == "rule_verifications" {
				target = 1
			}

			Must(t, db.Callback().Create().Before("gorm:create").
				Register(callbackName, func(tx *gorm.DB) {
					if tx.Statement.Schema == nil ||
						tx.Statement.Schema.Table != table {
						return
					}
					seen++
					if seen == target {
						injected = true
						tx.AddError(failure)
					}
				}))
			defer func() {
				if err := db.Callback().Create().Remove(callbackName); err != nil {
					t.Errorf("remove failure callback: %v", err)
				}
			}()

			if table == "rule_verifications" {
				result, callErr := svc.VerifyDraft(ctx, userID, verifyInput)
				if result != nil || !errors.Is(callErr, failure) {
					t.Fatalf("verification failure: result=%+v err=%v", result, callErr)
				}
			} else {
				result, callErr := svc.EditDraftWithVerification(ctx, edit)
				if result != nil || !errors.Is(callErr, failure) {
					t.Fatalf("edit failure: result=%+v err=%v", result, callErr)
				}
			}
			if !injected {
				t.Fatal("failure injection did not execute")
			}
			Equal(t, readVerificationState(t, observer, version.ID), before)
			Equal(t, Counts(t, observer), countsBefore)
		})
		if !ok {
			t.Fatal("rollback subtest failed; stop subsequent operations")
		}
	}

	// 仅构造本次版本的状态夹具，不代表发布/下线业务已验收。
	for _, status := range []model.RuleVersionStatus{
		model.RuleVersionStatusPublished,
		model.RuleVersionStatusWithdrawn,
	} {
		Must(t, db.Model(&model.RuleVersion{}).
			Where("id = ?", version.ID).Update("status", status).Error)
		before := readVerificationState(t, observer, version.ID)

		result, callErr := svc.EditDraftWithVerification(ctx, edit)
		if result != nil || !errors.Is(callErr, service.ErrKnowledgeVersionNotEditable) {
			t.Fatalf("edit status=%s result=%+v err=%v", status, result, callErr)
		}
		record, callErr := svc.VerifyDraft(ctx, userID, verifyInput)
		if record != nil || !errors.Is(callErr, service.ErrKnowledgeVersionNotEditable) {
			t.Fatalf("verify status=%s result=%+v err=%v", status, record, callErr)
		}
		Equal(t, readVerificationState(t, observer, version.ID), before)
	}

	t.Run("content_and_verification_boundaries", func(t *testing.T) {
		// 使用独立草稿，不改变前面事务、回滚和非草稿测试的夹具。
		boundaryInput := input
		boundaryInput.DocumentKey = key + "-boundary"

		draft, err := svc.CreateDraft(ctx, boundaryInput)
		Must(t, err)

		category := model.RuleCategory{
			Code: key + "-boundary",
			Name: "【虚构测试】修改后的分类",
		}
		Must(t, db.Create(&category).Error)
		boundaryCategoryID = category.ID

		before := readVerificationState(t, observer, draft.ID)
		start := time.Date(2026, 1, 2, 3, 4, 5, 123000000, time.UTC)
		end := start.Add(24 * time.Hour)

		change := service.EditKnowledgeDraftInput{
			VersionID:               draft.ID,
			ExpectedContentRevision: 1,
			CategoryID:              boundaryCategoryID,
			Title:                   "【虚构测试】修改全部内容字段",
			Content:                 "【虚构测试】修改后的原文\n新条款",
			SourceURL:               "https://www.bilibili.com/test-updated-only",
			EffectiveAt:             &start,
			ExpiresAt:               &end,
			Articles: []service.CreateRuleArticleInput{
				{
					SectionPath:   "【虚构测试】新章节",
					ArticleNumber: "新一",
					Content:       "【虚构测试】新条款",
				},
			},
		}

		result, err := svc.EditDraftWithVerification(ctx, change)
		Must(t, err)
		Equal(t, result.Version.ContentRevision, uint64(2))

		saved := readVerificationState(t, observer, draft.ID)
		Equal(t, saved.Version.CategoryID, boundaryCategoryID)
		Equal(t, saved.Version.Title, change.Title)
		Equal(t, saved.Version.Content, change.Content)
		Equal(t, saved.Version.SourceURL, change.SourceURL)
		if saved.Version.EffectiveAt == nil || saved.Version.ExpiresAt == nil {
			t.Fatal("edit lost effective or expiry time")
		}
		Equal(t, saved.Version.EffectiveAt.Equal(start), true)
		Equal(t, saved.Version.ExpiresAt.Equal(end), true)
		Equal(t, saved.Document, before.Document)
		Equal(t, len(saved.Articles), 1)
		Equal(t, len(saved.Chunks), 1)
		Equal(t, saved.Articles[0].VersionID, draft.ID)
		Equal(t, saved.Articles[0].ArticleOrder, uint32(1))
		Equal(t, saved.Articles[0].SectionPath, change.Articles[0].SectionPath)
		Equal(t, saved.Articles[0].ArticleNumber, change.Articles[0].ArticleNumber)
		Equal(t, saved.Articles[0].Content, change.Articles[0].Content)
		Equal(t, saved.Chunks[0].ArticleID, saved.Articles[0].ID)
		Equal(t, saved.Chunks[0].ChunkIndex, uint32(0))
		Equal(t, saved.Chunks[0].Content, saved.Articles[0].Content)

		// 即使所有内容相同，保存也是一次新的内容修订。
		change.ExpectedContentRevision = 2
		result, err = svc.EditDraftWithVerification(ctx, change)
		Must(t, err)
		Equal(t, result.Version.ContentRevision, uint64(3))
		saved = readVerificationState(t, observer, draft.ID)
		Equal(t, saved.Version.ContentRevision, uint64(3))
		Equal(t, saved.Version.Content, change.Content)
		Equal(t, saved.Document, before.Document)

		// ASCII链接的字节数与字符数相同，精确构造2048字符。
		baseURL := "https://www.bilibili.com/"
		longEvidence := baseURL + strings.Repeat("a", 2048-len(baseURL))
		longNote := strings.Repeat("核", 2000)
		check := service.VerifyKnowledgeDraftInput{
			VersionID:               draft.ID,
			ExpectedContentRevision: 3,
			VerificationNote:        longNote,
			EvidenceURL:             &longEvidence,
		}

		record, err := svc.VerifyDraft(ctx, userID, check)
		Must(t, err)

		saved = readVerificationState(t, observer, draft.ID)
		Equal(t, len(saved.Verifications), 1)
		stored := saved.Verifications[0]
		Equal(t, stored.ID, record.ID)
		Equal(t, stored.VerificationNote, longNote)
		Equal(t, stored.EvidenceURL, &longEvidence)
		Equal(t, stored.VerifiedBy, userID)
		Equal(t, stored.VerifiedContentRevision, uint64(3))
		Equal(t, stored.SnapshotSchemaVersion, uint32(2))

		// JSON对象不依赖键顺序；期望值来自本用例明确提交的内容。
		expectedSnapshot := map[string]any{
			"revision_type":       "unknown",
			"revision_note":       "",
			"revision_source_url": nil,
			"version_id":          draft.ID,
			"version":             boundaryInput.Version,
			"content_revision":    uint64(3),
			"category_id":         boundaryCategoryID,
			"title":               change.Title,
			"content":             change.Content,
			"source_url":          change.SourceURL,
			"effective_at":        start,
			"expires_at":          end,
			"articles": []map[string]any{
				{
					"article_order":  1,
					"section_path":   change.Articles[0].SectionPath,
					"article_number": change.Articles[0].ArticleNumber,
					"content":        change.Articles[0].Content,
				},
			},
		}
		var actualJSON, expectedJSON map[string]any
		Must(t, json.Unmarshal([]byte(stored.ContentSnapshot), &actualJSON))
		Must(t, json.Unmarshal([]byte(JSON(t, expectedSnapshot)), &expectedJSON))
		Equal(t, actualJSON, expectedJSON)

		// 超限请求不仅返回领域错误，还必须保持持久化状态完全不变。
		assertRejected := func(request service.VerifyKnowledgeDraftInput) {
			t.Helper()
			stateBefore := readVerificationState(t, observer, draft.ID)
			got, callErr := svc.VerifyDraft(ctx, userID, request)
			if got != nil || !errors.Is(callErr, service.ErrInvalidArgument) {
				t.Fatalf("expected invalid argument: result=%+v err=%v",
					got, callErr)
			}
			Equal(t, readVerificationState(t, observer, draft.ID), stateBefore)
		}

		tooLongNote := check
		tooLongNote.VerificationNote += "核"
		assertRejected(tooLongNote)

		tooLongURL := longEvidence + "a"
		tooLongEvidence := check
		tooLongEvidence.EvidenceURL = &tooLongURL
		assertRejected(tooLongEvidence)

		// 空白链接必须在实际数据库中保存为NULL。
		blank := " \t\n "
		check.VerificationNote = "【虚构测试】空白补充链接"
		check.EvidenceURL = &blank
		record, err = svc.VerifyDraft(ctx, userID, check)
		Must(t, err)
		saved = readVerificationState(t, observer, draft.ID)
		Equal(t, len(saved.Verifications), 2)
		Equal(t, saved.Verifications[1].ID, record.ID)
		Equal(t, saved.Verifications[1].EvidenceURL, (*string)(nil))
		Equal(t, saved.Verifications[0], stored)

		// 草稿录入允许普通合法URL；来源核验需要额外检查实际主来源。
		change.ExpectedContentRevision = 3
		change.SourceURL = "https://example.invalid/fictional-rule"
		result, err = svc.EditDraftWithVerification(ctx, change)
		Must(t, err)
		Equal(t, result.Version.ContentRevision, uint64(4))

		validEvidence := "https://www.bilibili.com/test-evidence-only"
		check.ExpectedContentRevision = 4
		check.EvidenceURL = &validEvidence
		check.VerificationNote = "【虚构测试】合法补充链接不能替代主来源"
		assertRejected(check)

		// 仅为本次夹具设置极限修订号，不模拟正常业务编辑。
		const maxRevision = ^uint64(0)
		update := db.Model(&model.RuleVersion{}).
			Where("id = ?", draft.ID).
			Update("content_revision", maxRevision)
		Must(t, update.Error)
		Equal(t, update.RowsAffected, int64(1))

		atLimit := readVerificationState(t, observer, draft.ID)
		Equal(t, atLimit.Version.ContentRevision, maxRevision)
		change.ExpectedContentRevision = maxRevision
		change.SourceURL = "https://www.bilibili.com/test-updated-only"

		result, err = svc.EditDraftWithVerification(ctx, change)
		if result != nil || !errors.Is(err, service.ErrKnowledgeContentConflict) {
			t.Fatalf("revision overflow must be rejected: result=%+v err=%v",
				result, err)
		}
		Equal(t, readVerificationState(t, observer, draft.ID), atLimit)

		t.Log("editable fields and snapshot match; same-content edit increments; " +
			"note/evidence limits persist correctly; invalid main source rejected; " +
			"maximum revision does not wrap")
	})

	t.Log("real MySQL: edit associations, verification snapshots, historical preservation, stale revisions, non-draft rejection and rollback checked")
}
