package router

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"video-support-agent/internal/model"
	"video-support-agent/internal/service"

	mysqlDriver "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// 所有规则均为虚构测试数据，只允许在已确认隔离的测试库运行。

func switchRevisionInput(
	old *model.RuleVersion, name string,
) service.CreateKnowledgeRevisionInput {
	evidence := "https://www.bilibili.com/test-revision-evidence"
	return service.CreateKnowledgeRevisionInput{
		DocumentID:        old.DocumentID,
		Version:           name,
		RevisionType:      model.RuleRevisionTypeOfficial,
		RevisionNote:      "【虚构测试】新版依据",
		RevisionSourceURL: &evidence,
		CategoryID:        old.CategoryID,
		Title:             "【虚构测试】新版标题",
		Content:           "【虚构测试】新版全文\n甲\n乙",
		SourceURL:         "https://www.bilibili.com/test-only",
		EffectiveAt:       old.EffectiveAt,
		ExpiresAt:         old.ExpiresAt,
		Articles: []service.CreateRuleArticleInput{
			{SectionPath: "第一章", ArticleNumber: "1", Content: "新版甲"},
			{SectionPath: "第二章", ArticleNumber: "", Content: "新版乙"},
		},
	}
}

func switchNewRevision(
	t *testing.T, f *publicationFixture,
	old *model.RuleVersion, name string, verified bool,
) *model.RuleVersion {
	t.Helper()
	v, err := f.svc(f.observer).CreateRevision(
		f.ctx, switchRevisionInput(old, name),
	)
	Must(t, err)
	if v == nil || v.ID == 0 || v.ID == old.ID {
		t.Fatal("new revision has no independent identity")
	}
	if verified {
		f.verify(t, v.ID, v.ContentRevision)
	}
	return v
}

func switchPublishInput(v *model.RuleVersion) service.PublishKnowledgeVersionInput {
	return service.PublishKnowledgeVersionInput{
		DocumentID:                  v.DocumentID,
		VersionID:                   v.ID,
		ExpectedPublicationRevision: 1,
	}
}

func switchBaseline(t *testing.T, f *publicationFixture) *model.RuleVersion {
	t.Helper()
	at := publicationTime().Add(-time.Hour)
	old := f.draft(t, &at, nil, true)
	result, err := f.svc(f.observer).PublishVersion(
		f.ctx, f.userID, publicationInput(old),
	)
	Must(t, err)
	f.published(t, old, result)
	return old
}

// 文档指针和发布修订号允许变化；历史版本内容及关联身份不能变化。
func switchWantFrozen(
	t *testing.T, got, want verificationDBState,
) {
	t.Helper()
	Equal(t, got.Version, want.Version)
	Equal(t, got.Articles, want.Articles)
	Equal(t, got.Chunks, want.Chunks)
	Equal(t, got.Verifications, want.Verifications)
}

func switchWantPublished(
	t *testing.T, f *publicationFixture,
	old, next *model.RuleVersion,
	result *service.KnowledgePublicationResult,
) {
	t.Helper()
	if result == nil {
		t.Fatal("nil switch publication result")
	}
	s := f.state(t, next.ID)
	at := time.Unix(0, f.now.Load()).UTC().Truncate(time.Millisecond)

	Equal(t, result.DocumentID, old.DocumentID)
	Equal(t, result.VersionID, next.ID)
	Equal(t, result.PublicationRevision, uint64(2))
	Equal(t, result.PublishedAt.Equal(at), true)
	Equal(t, s.Content.Document.CurrentVersionID, &next.ID)
	Equal(t, s.Content.Document.PublicationRevision, uint64(2))
	Equal(t, s.Content.Version.Status, model.RuleVersionStatusPublished)
	if s.Content.Version.PublishedAt == nil {
		t.Fatal("published version has no publication time")
	}
	Equal(t, s.Content.Version.PublishedAt.Equal(at), true)

	Equal(t, len(s.Operations), 2)
	first, last := s.Operations[0], s.Operations[1]
	Equal(t, first.AfterCurrentVersionID, &old.ID)
	Equal(t, first.AfterPublicationRevision, uint64(1))
	Equal(t, last.DocumentID, old.DocumentID)
	Equal(t, last.VersionID, next.ID)
	Equal(t, last.OperationType, model.RuleOperationTypePublish)
	Equal(t, last.BeforeCurrentVersionID, &old.ID)
	Equal(t, last.AfterCurrentVersionID, &next.ID)
	Equal(t, last.BeforePublicationRevision, uint64(1))
	Equal(t, last.AfterPublicationRevision, uint64(2))
	Equal(t, last.OperatorID, f.userID)
	Equal(t, last.OperatedAt.Equal(at), true)

	if len(s.Content.Verifications) == 0 {
		t.Fatal("switch succeeded without verification")
	}
	record := s.Content.Verifications[len(s.Content.Verifications)-1]
	Equal(t, record.VersionID, next.ID)
	Equal(t, record.VerifiedContentRevision, s.Content.Version.ContentRevision)
}

func TestKnowledgeSwitchMySQLCreateAndSnapshot(t *testing.T) {
	f := newPublicationFixture(t)
	old := switchBaseline(t, f)
	oldBefore := f.state(t, old.ID)
	input := switchRevisionInput(old, "revision-one")

	v, err := f.svc(f.db).CreateRevision(f.ctx, input)
	Must(t, err)
	if v == nil || v.ID == 0 || v.ID == old.ID {
		t.Fatal("invalid new revision identity")
	}
	saved := f.state(t, v.ID)
	Equal(t, saved.Content.Document, oldBefore.Content.Document)
	Equal(t, saved.Operations, oldBefore.Operations)
	Equal(t, saved.Content.Version.Status, model.RuleVersionStatusDraft)
	Equal(t, saved.Content.Version.ContentRevision, uint64(1))
	Equal(t, saved.Content.Version.RevisionType, input.RevisionType)
	Equal(t, saved.Content.Version.RevisionNote, input.RevisionNote)
	Equal(t, saved.Content.Version.RevisionSourceURL, input.RevisionSourceURL)
	Equal(t, saved.Content.Version.Content, input.Content)
	Equal(t, len(saved.Content.Verifications), 0)
	Equal(t, len(saved.Content.Articles), 2)
	Equal(t, len(saved.Content.Chunks), 2)

	for i, article := range saved.Content.Articles {
		Equal(t, article.VersionID, v.ID)
		Equal(t, article.ArticleOrder, uint32(i+1))
		Equal(t, article.SectionPath, input.Articles[i].SectionPath)
		Equal(t, article.ArticleNumber, input.Articles[i].ArticleNumber)
		Equal(t, article.Content, input.Articles[i].Content)
		for _, oldArticle := range oldBefore.Content.Articles {
			if article.ID == oldArticle.ID {
				t.Fatal("new revision reused historical article ID")
			}
		}
		chunk := saved.Content.Chunks[i]
		Equal(t, chunk.ArticleID, article.ID)
		Equal(t, chunk.ChunkIndex, uint32(0))
		Equal(t, chunk.Content, article.Content)
	}
	Equal(t, f.state(t, old.ID), oldBefore)

	// 同文档版本名重复必须失败，且不留下额外记录。
	counts := Counts(t, f.observer)
	duplicate, err := f.svc(f.db).CreateRevision(f.ctx, input)
	if duplicate != nil || !errors.Is(err, service.ErrKnowledgeVersionExists) {
		t.Fatalf("duplicate result=%+v error=%v", duplicate, err)
	}
	Equal(t, Counts(t, f.observer), counts)
	Equal(t, f.state(t, v.ID), saved)

	// 数据库层文档、分类存在性检查。
	for _, target := range []string{"document", "category"} {
		bad := input
		bad.Version = "missing-" + target
		var count int64
		want := service.ErrKnowledgeDocumentNotFound
		if target == "document" {
			Must(t, f.observer.Model(&model.KnowledgeDocument{}).
				Where("id = ?", ^uint64(0)).Count(&count).Error)
			bad.DocumentID = ^uint64(0)
		} else {
			Must(t, f.observer.Model(&model.RuleCategory{}).
				Where("id = ?", ^uint64(0)).Count(&count).Error)
			bad.CategoryID = ^uint64(0)
			want = service.ErrRuleCategoryNotFound
		}
		Equal(t, count, int64(0))
		got, callErr := f.svc(f.db).CreateRevision(f.ctx, bad)
		if got != nil || !errors.Is(callErr, want) {
			t.Fatalf("%s: result=%+v error=%v", target, got, callErr)
		}
		Equal(t, Counts(t, f.observer), counts)
		Equal(t, f.state(t, old.ID), oldBefore)
	}

	// 相同版本名在另一逻辑文档下允许使用。
	other := f.draft(t, old.EffectiveAt, nil, false)
	otherInput := input
	otherInput.DocumentID = other.DocumentID
	otherVersion, err := f.svc(f.db).CreateRevision(f.ctx, otherInput)
	Must(t, err)
	Equal(t, otherVersion.DocumentID, other.DocumentID)

	f.verify(t, v.ID, 1)
	verified := f.state(t, v.ID)
	Equal(t, len(verified.Content.Verifications), 1)
	record := verified.Content.Verifications[0]
	Equal(t, record.SnapshotSchemaVersion, uint32(2))
	Equal(t, record.VerifiedBy, f.userID)

	var snapshot map[string]any
	Must(t, json.Unmarshal([]byte(record.ContentSnapshot), &snapshot))
	expected := map[string]any{
		"version_id": v.ID, "version": input.Version,
		"content_revision": 1, "category_id": input.CategoryID,
		"revision_type": input.RevisionType, "revision_note": input.RevisionNote,
		"revision_source_url": input.RevisionSourceURL,
		"title":               input.Title, "content": input.Content,
		"source_url":   input.SourceURL,
		"effective_at": input.EffectiveAt, "expires_at": input.ExpiresAt,
		"articles": []map[string]any{
			{"article_order": 1, "section_path": "第一章", "article_number": "1", "content": "新版甲"},
			{"article_order": 2, "section_path": "第二章", "article_number": "", "content": "新版乙"},
		},
	}
	var expectedJSON map[string]any
	Must(t, json.Unmarshal([]byte(JSON(t, expected)), &expectedJSON))
	Equal(t, snapshot, expectedJSON)

	// 测试夹具模拟已经存量存在的格式1记录，不修改任何生产数据。
	delete(snapshot, "revision_type")
	delete(snapshot, "revision_note")
	delete(snapshot, "revision_source_url")
	Must(t, f.observer.Model(&model.RuleVerification{}).
		Where("id = ?", record.ID).
		Updates(map[string]any{
			"snapshot_schema_version": 1,
			"content_snapshot":        JSON(t, snapshot),
		}).Error)
	legacy := f.state(t, v.ID).Content.Verifications[0]
	Equal(t, legacy.SnapshotSchemaVersion, uint32(1))

	// 再核验生成格式2；旧格式1的记录及快照保持原样。
	f.verify(t, v.ID, 1)
	records := f.state(t, v.ID).Content.Verifications
	Equal(t, len(records), 2)
	Equal(t, records[0], legacy)
	Equal(t, records[1].SnapshotSchemaVersion, uint32(2))
	Equal(t, records[1].VerifiedContentRevision, uint64(1))
	Equal(t, f.state(t, old.ID), oldBefore)
}

func TestKnowledgeSwitchMySQLRollback(t *testing.T) {
	for _, operation := range []string{"create", "publish"} {
		tables := []string{"rule_versions", "rule_articles", "knowledge_chunks"}
		if operation == "publish" {
			tables = []string{"rule_versions", "knowledge_documents", "rule_operations"}
		}
		for _, table := range tables {
			t.Run(operation+"_"+table, func(t *testing.T) {
				f := newPublicationFixture(t)
				old := switchBaseline(t, f)
				var next *model.RuleVersion
				if operation == "publish" {
					next = switchNewRevision(t, f, old, "next", true)
				}
				oldBefore := f.state(t, old.ID)
				var nextBefore publicationDBState
				if next != nil {
					nextBefore = f.state(t, next.ID)
				}
				counts := Counts(t, f.observer)
				failure := errors.New("controlled switch write failure")
				injected := false
				name := f.prefix + "_switch_fault"

				callback := func(tx *gorm.DB) {
					if tx.Error != nil || tx.Statement.Schema == nil ||
						tx.Statement.Schema.Table != table {
						return
					}
					if tx.RowsAffected < 1 {
						tx.AddError(errors.New("fault point reached without successful write"))
						return
					}
					injected = true
					tx.AddError(failure)
				}
				if operation == "create" || table == "rule_operations" {
					Must(t, f.db.Callback().Create().
						After("gorm:create").Register(name, callback))
					t.Cleanup(func() {
						Must(t, f.db.Callback().Create().Remove(name))
					})
				} else {
					Must(t, f.db.Callback().Update().
						After("gorm:update").Register(name, callback))
					t.Cleanup(func() {
						Must(t, f.db.Callback().Update().Remove(name))
					})
				}

				var callErr error
				if operation == "create" {
					got, err := f.svc(f.db).CreateRevision(
						f.ctx, switchRevisionInput(old, "failed-create"),
					)
					callErr = err
					if got != nil {
						t.Fatal("failed creation returned a version")
					}
				} else {
					got, err := f.svc(f.db).PublishVersion(
						f.ctx, f.userID, switchPublishInput(next),
					)
					callErr = err
					if got != nil {
						t.Fatal("failed switch returned a publication")
					}
				}
				if !injected || !errors.Is(callErr, failure) {
					t.Fatalf("injected=%v error=%v", injected, callErr)
				}

				// 独立observer连接验证提交后的实际状态。
				Equal(t, Counts(t, f.observer), counts)
				Equal(t, f.state(t, old.ID), oldBefore)
				if next != nil {
					Equal(t, f.state(t, next.ID), nextBefore)
				}
				t.Logf("independent observer confirmed rollback: %s/%s", operation, table)
			})
		}
	}
}

func TestKnowledgeSwitchMySQLHistory(t *testing.T) {
	f := newPublicationFixture(t)
	now := publicationTime()
	oldEnd := now.Add(3 * time.Hour)
	old := f.draft(t, &now, &oldEnd, true)
	first, err := f.svc(f.db).PublishVersion(
		f.ctx, f.userID, publicationInput(old),
	)
	Must(t, err)
	f.published(t, old, first)
	oldBefore := f.state(t, old.ID)

	input := switchRevisionInput(old, "next")
	newEnd := now.Add(time.Hour)
	input.ExpiresAt = &newEnd
	next, err := f.svc(f.db).CreateRevision(f.ctx, input)
	Must(t, err)
	draftState := f.state(t, next.ID)

	// 已核验也仍然是草稿，公开历史不能读取。
	f.verify(t, next.ID, 1)
	detail, err := f.svc(f.db).GetHistoryByVersionID(f.ctx, next.ID)
	if detail != nil || !errors.Is(err, service.ErrKnowledgeVersionNotFound) {
		t.Fatalf("draft history=%+v error=%v", detail, err)
	}
	article, err := f.svc(f.db).GetHistoryArticle(
		f.ctx, next.ID, draftState.Content.Articles[0].ID,
	)
	if article != nil || !errors.Is(err, service.ErrKnowledgeVersionNotFound) {
		t.Fatalf("draft article=%+v error=%v", article, err)
	}

	second, err := f.svc(f.db).PublishVersion(
		f.ctx, f.userID, switchPublishInput(next),
	)
	Must(t, err)
	switchWantPublished(t, f, old, next, second)
	switchWantFrozen(t, f.state(t, old.ID).Content, oldBefore.Content)

	history, err := f.svc(f.db).GetHistoryByVersionID(f.ctx, old.ID)
	Must(t, err)
	Equal(t, history.Articles, oldBefore.Content.Articles)
	Equal(t, history.Version.Content, oldBefore.Content.Version.Content)
	Equal(t, history.State, service.KnowledgeHistoryState{IsSuperseded: true})

	current, err := f.svc(f.db).GetHistoryByVersionID(f.ctx, next.ID)
	Must(t, err)
	Equal(t, current.State, service.KnowledgeHistoryState{
		IsCurrent: true, IsCurrentEffective: true,
	})

	oldArticle := oldBefore.Content.Articles[0]
	article, err = f.svc(f.db).GetHistoryArticle(f.ctx, old.ID, oldArticle.ID)
	Must(t, err)
	Equal(t, *article.Article, oldArticle)
	Equal(t, article.Version.SourceURL, old.SourceURL)

	// 真实SQL必须联合过滤version_id与article_id。
	article, err = f.svc(f.db).GetHistoryArticle(f.ctx, next.ID, oldArticle.ID)
	if article != nil || !errors.Is(err, service.ErrKnowledgeArticleNotFound) {
		t.Fatalf("cross-version article=%+v error=%v", article, err)
	}

	// 禁止直接恢复已被替代的历史版本。
	restore := service.PublishKnowledgeVersionInput{
		DocumentID: old.DocumentID, VersionID: old.ID,
		ExpectedPublicationRevision: 2,
	}
	beforeRestore := f.state(t, old.ID)
	got, err := f.svc(f.db).PublishVersion(f.ctx, f.userID, restore)
	if got != nil || !errors.Is(err, service.ErrKnowledgeVersionNotPublishable) {
		t.Fatalf("restore result=%+v error=%v", got, err)
	}
	Equal(t, f.state(t, old.ID), beforeRestore)

	// 新版截止时刻已过期，旧版此时仍在有效期，也不得回退。
	f.now.Store(newEnd.UnixNano())
	current, err = f.svc(f.db).GetHistoryByVersionID(f.ctx, next.ID)
	Must(t, err)
	Equal(t, current.State, service.KnowledgeHistoryState{
		IsCurrent: true, IsExpired: true,
	})
	effective, err := f.svc(f.db).GetCurrentByDocumentID(f.ctx, old.DocumentID)
	if effective != nil || !errors.Is(err, service.ErrKnowledgeCurrentVersionNotFound) {
		t.Fatalf("unexpected fallback=%+v error=%v", effective, err)
	}

	// 测试专用夹具构造历史下线状态；不代表S05下线业务已验收。
	Must(t, f.observer.WithContext(f.ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&model.RuleVersion{}).Where("id = ?", old.ID).
			Updates(map[string]any{
				"status": model.RuleVersionStatusWithdrawn, "withdrawn_at": newEnd,
			}).Error; err != nil {
			return err
		}
		return tx.Model(&model.KnowledgeDocument{}).
			Where("id = ?", old.DocumentID).
			Update("current_version_id", nil).Error
	}))
	f.now.Store(oldEnd.UnixNano())
	history, err = f.svc(f.db).GetHistoryByVersionID(f.ctx, old.ID)
	Must(t, err)
	Equal(t, history.State, service.KnowledgeHistoryState{
		IsSuperseded: true, IsExpired: true, IsWithdrawn: true,
	})
	Equal(t, history.Articles, oldBefore.Content.Articles)
	Equal(t, history.Version.Content, oldBefore.Content.Version.Content)
}

func TestKnowledgeSwitchMySQLHistorySnapshot(t *testing.T) {
	f := newPublicationFixture(t)
	old := switchBaseline(t, f)
	next := switchNewRevision(t, f, old, "next", true)
	name := f.prefix + "_history_snapshot"
	fired := false

	// 历史读取已经读到旧版本后，另一个连接提交新版发布。
	Must(t, f.db.Callback().Query().After("gorm:query").
		Register(name, func(tx *gorm.DB) {
			if fired || tx.Error != nil || tx.Statement.Schema == nil ||
				tx.Statement.Schema.Table != "rule_versions" {
				return
			}
			fired = true
			_, err := f.svc(f.observer).PublishVersion(
				f.ctx, f.userID, switchPublishInput(next),
			)
			if err != nil {
				tx.AddError(err)
			}
		}))
	t.Cleanup(func() { Must(t, f.db.Callback().Query().Remove(name)) })

	history, err := f.svc(f.db).GetHistoryByVersionID(f.ctx, old.ID)
	Must(t, err)
	if !fired {
		t.Fatal("snapshot interleaving did not execute")
	}
	Equal(t, history.State, service.KnowledgeHistoryState{
		IsCurrent: true, IsCurrentEffective: true,
	})

	fresh, err := f.svc(f.second).GetHistoryByVersionID(f.ctx, old.ID)
	Must(t, err)
	Equal(t, fresh.State, service.KnowledgeHistoryState{IsSuperseded: true})
	Equal(t, fresh.Articles, history.Articles)
	t.Log("same read kept its snapshot; fresh reader observed committed replacement")
}

func TestKnowledgeSwitchMySQLConcurrency(t *testing.T) {
	for _, scenario := range []string{
		"publish_first_candidate",
		"publish_second_candidate",
		"edit_then_publish",
		"publish_then_edit",
	} {
		t.Run(scenario, func(t *testing.T) {
			f := newPublicationFixture(t)
			old := switchBaseline(t, f)
			firstVersion := switchNewRevision(t, f, old, "candidate-one", true)
			secondVersion := firstVersion
			firstOp, secondOp := "publish", "publish"

			switch scenario {
			case "publish_first_candidate", "publish_second_candidate":
				secondVersion = switchNewRevision(t, f, old, "candidate-two", true)
				if scenario == "publish_second_candidate" {
					firstVersion, secondVersion = secondVersion, firstVersion
				}
			case "edit_then_publish":
				firstOp = "edit"
			case "publish_then_edit":
				secondOp = "edit"
			}

			oldBefore := f.state(t, old.ID)
			firstBefore := f.state(t, firstVersion.ID)
			secondBefore := f.state(t, secondVersion.ID)

			locked := make(chan struct{})
			entering := make(chan struct{})
			releaseCh := make(chan struct{})
			var firstOnce, secondOnce, releaseOnce sync.Once
			var workers sync.WaitGroup
			release := func() { releaseOnce.Do(func() { close(releaseCh) }) }
			defer func() {
				release()
				workers.Wait()
			}()

			firstName := f.prefix + "_switch_first"
			Must(t, f.db.Callback().Query().After("gorm:query").
				Register(firstName, func(tx *gorm.DB) {
					if tx.Error != nil || tx.Statement.Schema == nil ||
						tx.Statement.Schema.Table != "rule_versions" {
						return
					}
					if _, ok := tx.Statement.Clauses["FOR"]; !ok {
						return
					}
					firstOnce.Do(func() {
						close(locked)
						select {
						case <-releaseCh:
						case <-f.ctx.Done():
							tx.AddError(f.ctx.Err())
						}
					})
				}))
			t.Cleanup(func() {
				Must(t, f.db.Callback().Query().Remove(firstName))
			})

			secondTable := "rule_versions"
			if firstOp == "publish" && secondOp == "publish" {
				secondTable = "knowledge_documents"
			}
			secondName := f.prefix + "_switch_second"
			Must(t, f.second.Callback().Query().Before("gorm:query").
				Register(secondName, func(tx *gorm.DB) {
					if tx.Statement.Schema == nil ||
						tx.Statement.Schema.Table != secondTable {
						return
					}
					if _, ok := tx.Statement.Clauses["FOR"]; !ok {
						return
					}
					secondOnce.Do(func() { close(entering) })
				}))
			t.Cleanup(func() {
				Must(t, f.second.Callback().Query().Remove(secondName))
			})

			type outcome struct {
				publication *service.KnowledgePublicationResult
				edit        *service.KnowledgeDraftEditResult
				err         error
			}
			start := func(db *gorm.DB, op string, v *model.RuleVersion) <-chan outcome {
				done := make(chan outcome, 1)
				workers.Add(1)
				go func() {
					defer workers.Done()
					var result outcome
					if op == "publish" {
						result.publication, result.err = f.svc(db).PublishVersion(
							f.ctx, f.userID, switchPublishInput(v),
						)
					} else {
						result.edit, result.err = f.svc(db).EditDraftWithVerification(
							f.ctx, publicationEdit(v),
						)
					}
					done <- result
				}()
				return done
			}
			awaitSignal := func(ch <-chan struct{}) {
				t.Helper()
				select {
				case <-ch:
				case <-f.ctx.Done():
					t.Fatal("competition coordination timed out")
				}
			}
			awaitResult := func(ch <-chan outcome) outcome {
				t.Helper()
				select {
				case result := <-ch:
					return result
				case <-f.ctx.Done():
					t.Fatal("competition result timed out")
					return outcome{}
				}
			}

			firstDone := start(f.db, firstOp, firstVersion)
			awaitSignal(locked)

			probe := func(value any, id uint64) {
				t.Helper()
				err := f.observer.WithContext(f.ctx).Transaction(func(tx *gorm.DB) error {
					return tx.Clauses(clause.Locking{
						Strength: "UPDATE", Options: "NOWAIT",
					}).Where("id = ?", id).First(value).Error
				})
				var mysqlErr *mysqlDriver.MySQLError
				if !errors.As(err, &mysqlErr) || mysqlErr.Number != 3572 {
					t.Fatalf("expected actual NOWAIT error 3572, got %v", err)
				}
			}
			probe(&model.RuleVersion{}, firstVersion.ID)
			if firstOp == "publish" {
				probe(&model.KnowledgeDocument{}, old.DocumentID)
			}

			secondDone := start(f.second, secondOp, secondVersion)
			awaitSignal(entering)
			release()

			first := awaitResult(firstDone)
			second := awaitResult(secondDone)
			workers.Wait()
			Must(t, first.err)

			want := service.ErrKnowledgePublicationConflict
			if firstOp == "edit" {
				want = service.ErrKnowledgeVerificationRequired
			} else if secondOp == "edit" {
				want = service.ErrKnowledgeVersionNotEditable
			}
			if !errors.Is(second.err, want) ||
				second.publication != nil || second.edit != nil {
				t.Fatalf("second=%+v want=%v", second, want)
			}

			if firstOp == "publish" {
				switchWantPublished(t, f, old, firstVersion, first.publication)
				after := f.state(t, firstVersion.ID)
				Equal(t, after.Content.Articles, firstBefore.Content.Articles)
				Equal(t, after.Content.Chunks, firstBefore.Content.Chunks)
				Equal(t, after.Content.Verifications, firstBefore.Content.Verifications)
				Equal(t, after.Content.Version.ContentRevision, uint64(1))
				Equal(t, after.Content.Version.Content, firstBefore.Content.Version.Content)
				if secondVersion.ID != firstVersion.ID {
					switchWantFrozen(t,
						f.state(t, secondVersion.ID).Content,
						secondBefore.Content,
					)
				}
			} else {
				if first.edit == nil || first.edit.Version == nil {
					t.Fatal("successful edit returned no version")
				}
				after := f.state(t, firstVersion.ID)
				Equal(t, first.edit.Version.ContentRevision, uint64(2))
				Equal(t, after.Content.Version.ContentRevision, uint64(2))
				Equal(t, after.Content.Version.Status, model.RuleVersionStatusDraft)
				Equal(t, after.Content.Document, oldBefore.Content.Document)
				Equal(t, after.Operations, oldBefore.Operations)
				Equal(t, len(after.Content.Articles), 1)
				Equal(t, len(after.Content.Chunks), 1)
				Equal(t, after.Content.Chunks[0].ArticleID, after.Content.Articles[0].ID)
				Equal(t, after.Content.Chunks[0].Content, "修改条款")
				Equal(t, after.Content.Verifications, firstBefore.Content.Verifications)
				Equal(t, after.Content.Verifications[0].VerifiedContentRevision, uint64(1))
			}
			switchWantFrozen(t, f.state(t, old.ID).Content, oldBefore.Content)
			t.Logf("real locks confirmed; %s succeeded, %s rejected with %v",
				firstOp, secondOp, want)
		})
	}
}

// 保留fmt导入的实际用途，并为关键夹具信息提供清晰输出。
func TestKnowledgeSwitchMySQLIdentitySummary(t *testing.T) {
	f := newPublicationFixture(t)
	t.Log(fmt.Sprintf("isolated fixture initialized: %s", f.prefix))
}
