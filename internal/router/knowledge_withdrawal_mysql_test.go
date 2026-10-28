package router

import (
	"errors"
	"sync"
	"testing"
	"time"

	"video-support-agent/internal/model"
	"video-support-agent/internal/service"

	mysqlDriver "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// 全部规则为虚构测试数据。
// newPublicationFixture在写入前验证库名、实例UUID及独立连接，
// 并只清理本次随机文档键及自有用户、分类。
func withdrawalPair(
	t *testing.T,
	f *publicationFixture,
	expires *time.Time,
) (*model.RuleVersion, *model.RuleVersion) {
	t.Helper()
	old := switchBaseline(t, f)

	input := switchRevisionInput(old, "withdraw-current")
	input.ExpiresAt = expires
	current, err := f.svc(f.observer).CreateRevision(f.ctx, input)
	Must(t, err)
	if current == nil {
		t.Fatal("revision returned nil")
	}
	f.verify(t, current.ID, current.ContentRevision)

	result, err := f.svc(f.observer).PublishVersion(
		f.ctx, f.userID, switchPublishInput(current),
	)
	Must(t, err)
	switchWantPublished(t, f, old, current, result)
	return old, current
}

func withdrawalInput(
	v *model.RuleVersion, revision uint64,
) service.WithdrawKnowledgeVersionInput {
	return service.WithdrawKnowledgeVersionInput{
		DocumentID:                  v.DocumentID,
		VersionID:                   v.ID,
		ExpectedPublicationRevision: revision,
	}
}

func withdrawalWantNoCurrent(
	t *testing.T, f *publicationFixture, documentID uint64,
) {
	t.Helper()
	got, err := f.svc(f.observer).GetCurrentByDocumentID(f.ctx, documentID)
	if got != nil || !errors.Is(err, service.ErrKnowledgeCurrentVersionNotFound) {
		t.Fatalf("current=%+v err=%v; expected no effective version", got, err)
	}
}

// 只允许本次下线契约规定的字段变化。
// 原始内容、内容修订、发布时间及关联记录全部比较。
func withdrawalWantCommitted(
	t *testing.T,
	f *publicationFixture,
	before publicationDBState,
	result *service.KnowledgeWithdrawalResult,
) {
	t.Helper()
	if result == nil {
		t.Fatal("successful withdrawal returned nil")
	}
	after := f.state(t, before.Content.Version.ID)
	at := time.Unix(0, f.now.Load()).UTC().Truncate(time.Millisecond)
	nextRevision := before.Content.Document.PublicationRevision + 1

	Equal(t, result.DocumentID, before.Content.Document.ID)
	Equal(t, result.VersionID, before.Content.Version.ID)
	Equal(t, result.PublicationRevision, nextRevision)
	Equal(t, result.WithdrawnAt.Equal(at), true)

	wantDocument := before.Content.Document
	wantDocument.CurrentVersionID = nil
	wantDocument.PublicationRevision = nextRevision
	wantDocument.UpdatedAt = after.Content.Document.UpdatedAt
	Equal(t, after.Content.Document.UpdatedAt.Equal(at), true)
	Equal(t, after.Content.Document, wantDocument)

	if after.Content.Version.WithdrawnAt == nil {
		t.Fatal("withdrawn version has no withdrawal time")
	}
	Equal(t, after.Content.Version.WithdrawnAt.Equal(at), true)
	Equal(t, after.Content.Version.UpdatedAt.Equal(at), true)

	wantVersion := before.Content.Version
	wantVersion.Status = model.RuleVersionStatusWithdrawn
	wantVersion.WithdrawnAt = after.Content.Version.WithdrawnAt
	wantVersion.UpdatedAt = after.Content.Version.UpdatedAt
	Equal(t, after.Content.Version, wantVersion)
	Equal(t, after.Content.Articles, before.Content.Articles)
	Equal(t, after.Content.Chunks, before.Content.Chunks)
	Equal(t, after.Content.Verifications, before.Content.Verifications)

	Equal(t, len(after.Operations), len(before.Operations)+1)
	Equal(t, after.Operations[:len(before.Operations)], before.Operations)
	op := after.Operations[len(before.Operations)]
	if op.ID == 0 {
		t.Fatal("withdraw operation has no saved identity")
	}
	target := before.Content.Version.ID
	Equal(t, op.DocumentID, before.Content.Document.ID)
	Equal(t, op.VersionID, target)
	Equal(t, op.OperationType, model.RuleOperationTypeWithdraw)
	Equal(t, op.BeforeCurrentVersionID, &target)
	Equal(t, op.AfterCurrentVersionID, (*uint64)(nil))
	Equal(t, op.BeforePublicationRevision, nextRevision-1)
	Equal(t, op.AfterPublicationRevision, nextRevision)
	Equal(t, op.OperatorID, f.userID)
	Equal(t, op.OperatedAt.Equal(at), true)
}

func TestKnowledgeWithdrawalMySQLSuccessAndHistory(t *testing.T) {
	f := newPublicationFixture(t)
	old, current := withdrawalPair(t, f, nil)
	oldBefore := f.state(t, old.ID)
	before := f.state(t, current.ID)

	f.now.Store(publicationTime().Add(time.Minute).UnixNano())
	calls := f.calls.Load()
	result, err := f.svc(f.db).WithdrawVersion(
		f.ctx, f.userID, withdrawalInput(current, 2),
	)
	Must(t, err)
	Equal(t, f.calls.Load()-calls, int64(1))
	withdrawalWantCommitted(t, f, before, result)
	switchWantFrozen(t, f.state(t, old.ID).Content, oldBefore.Content)
	withdrawalWantNoCurrent(t, f, old.DocumentID)

	for _, version := range []*model.RuleVersion{old, current} {
		saved := f.state(t, version.ID)
		history, err := f.svc(f.db).GetHistoryByVersionID(f.ctx, version.ID)
		Must(t, err)
		if history == nil || history.Version == nil {
			t.Fatal("historical version disappeared")
		}
		Equal(t, *history.Version, saved.Content.Version)
		Equal(t, history.Articles, saved.Content.Articles)

		wantState := service.KnowledgeHistoryState{IsWithdrawn: true}
		if version.ID == old.ID {
			wantState = service.KnowledgeHistoryState{IsSuperseded: true}
		}
		Equal(t, history.State, wantState)

		for _, article := range saved.Content.Articles {
			detail, err := f.svc(f.db).GetHistoryArticle(
				f.ctx, version.ID, article.ID,
			)
			Must(t, err)
			if detail == nil || detail.Article == nil || detail.Version == nil {
				t.Fatal("historical article disappeared")
			}
			Equal(t, *detail.Article, article)
			Equal(t, *detail.Version, saved.Content.Version)
			Equal(t, detail.State, wantState)
		}
	}

	// 旧请求和刷新修订后的重复请求是不同冲突。
	after := f.state(t, current.ID)
	counts := Counts(t, f.observer)
	for _, tc := range []struct {
		revision uint64
		want     error
	}{
		{2, service.ErrKnowledgePublicationConflict},
		{3, service.ErrKnowledgeVersionNotWithdrawable},
	} {
		got, err := f.svc(f.db).WithdrawVersion(
			f.ctx, f.userID, withdrawalInput(current, tc.revision),
		)
		if got != nil || !errors.Is(err, tc.want) {
			t.Fatalf("repeat revision=%d result=%+v err=%v",
				tc.revision, got, err)
		}
		Equal(t, f.state(t, current.ID), after)
		Equal(t, Counts(t, f.observer), counts)
	}

	// 清空指针不能成为旧版或已下线版本的一键恢复入口。
	for _, version := range []*model.RuleVersion{old, current} {
		got, err := f.svc(f.db).PublishVersion(
			f.ctx, f.userID, service.PublishKnowledgeVersionInput{
				DocumentID:                  old.DocumentID,
				VersionID:                   version.ID,
				ExpectedPublicationRevision: 3,
			},
		)
		if got != nil || !errors.Is(err, service.ErrKnowledgeVersionNotPublishable) {
			t.Fatalf("restore result=%+v err=%v", got, err)
		}
		Equal(t, f.state(t, current.ID), after)
		Equal(t, Counts(t, f.observer), counts)
	}
}

func TestKnowledgeWithdrawalMySQLRejects(t *testing.T) {
	for _, scenario := range []string{
		"wrong_owner",
		"noncurrent",
		"draft",
		"missing_document",
		"missing_version",
		"stale_revision",
		"revision_overflow",
		"missing_published_at",
		"already_has_withdrawn_at",
		"withdrawn_status",
	} {
		t.Run(scenario, func(t *testing.T) {
			f := newPublicationFixture(t)
			old, current := withdrawalPair(t, f, nil)
			input := withdrawalInput(current, 2)
			want := service.ErrKnowledgeVersionNotWithdrawable
			var extra *model.RuleVersion

			switch scenario {
			case "wrong_owner":
				extra = f.draft(t, old.EffectiveAt, nil, false)
				input.VersionID = extra.ID
				want = service.ErrKnowledgeVersionNotFound
			case "noncurrent":
				input.VersionID = old.ID
			case "draft":
				extra = switchNewRevision(t, f, old, "draft-target", false)
				input.VersionID = extra.ID
			case "missing_document":
				var count int64
				Must(t, f.observer.Model(&model.KnowledgeDocument{}).
					Where("id = ?", ^uint64(0)).Count(&count).Error)
				Equal(t, count, int64(0))
				input.DocumentID = ^uint64(0)
				want = service.ErrKnowledgeDocumentNotFound
			case "missing_version":
				var count int64
				Must(t, f.observer.Model(&model.RuleVersion{}).
					Where("id = ?", ^uint64(0)).Count(&count).Error)
				Equal(t, count, int64(0))
				input.VersionID = ^uint64(0)
				want = service.ErrKnowledgeVersionNotFound
			case "stale_revision":
				input.ExpectedPublicationRevision = 1
				want = service.ErrKnowledgePublicationConflict
			case "revision_overflow":
				// 仅在自有测试夹具构造无法通过正常操作达到的边界。
				Must(t, f.observer.Model(&model.KnowledgeDocument{}).
					Where("id = ?", current.DocumentID).
					Update("publication_revision", ^uint64(0)).Error)
				input.ExpectedPublicationRevision = ^uint64(0)
				want = service.ErrKnowledgePublicationConflict
			case "missing_published_at":
				Must(t, f.observer.Model(&model.RuleVersion{}).
					Where("id = ?", current.ID).
					Update("published_at", nil).Error)
			case "already_has_withdrawn_at":
				Must(t, f.observer.Model(&model.RuleVersion{}).
					Where("id = ?", current.ID).
					Update("withdrawn_at", publicationTime()).Error)
			case "withdrawn_status":
				Must(t, f.observer.Model(&model.RuleVersion{}).
					Where("id = ?", current.ID).
					Update("status", model.RuleVersionStatusWithdrawn).Error)
			}

			before := f.state(t, current.ID)
			oldBefore := f.state(t, old.ID)
			var extraBefore publicationDBState
			if extra != nil {
				extraBefore = f.state(t, extra.ID)
			}
			counts := Counts(t, f.observer)

			got, err := f.svc(f.db).WithdrawVersion(f.ctx, f.userID, input)
			if got != nil || !errors.Is(err, want) {
				t.Fatalf("result=%+v err=%v want=%v", got, err, want)
			}
			Equal(t, f.state(t, current.ID), before)
			Equal(t, f.state(t, old.ID), oldBefore)
			if extra != nil {
				Equal(t, f.state(t, extra.ID), extraBefore)
			}
			Equal(t, Counts(t, f.observer), counts)
		})
	}
}

func TestKnowledgeWithdrawalMySQLRollback(t *testing.T) {
	for _, table := range []string{
		"rule_versions", "knowledge_documents", "rule_operations",
	} {
		t.Run(table, func(t *testing.T) {
			f := newPublicationFixture(t)
			old, current := withdrawalPair(t, f, nil)
			oldBefore := f.state(t, old.ID)
			before := f.state(t, current.ID)
			counts := Counts(t, f.observer)

			failure := errors.New("controlled withdrawal write failure")
			injected := false
			name := f.prefix + "_withdraw_fault"
			callback := func(tx *gorm.DB) {
				if tx.Error != nil || tx.Statement.Schema == nil ||
					tx.Statement.Schema.Table != table {
					return
				}
				if tx.RowsAffected != 1 {
					tx.AddError(errors.New("fault point did not follow one successful write"))
					return
				}
				injected = true
				tx.AddError(failure)
			}

			if table == "rule_operations" {
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

			got, err := f.svc(f.db).WithdrawVersion(
				f.ctx, f.userID, withdrawalInput(current, 2),
			)
			if got != nil || !injected || !errors.Is(err, failure) {
				t.Fatalf("result=%+v injected=%v err=%v", got, injected, err)
			}

			Equal(t, f.state(t, current.ID), before)
			Equal(t, f.state(t, old.ID), oldBefore)
			Equal(t, Counts(t, f.observer), counts)
			effective, err := f.svc(f.observer).
				GetCurrentByDocumentID(f.ctx, current.DocumentID)
			Must(t, err)
			if effective == nil || effective.Version == nil {
				t.Fatal("rollback lost the current version")
			}
			Equal(t, effective.Version.ID, current.ID)
			t.Logf("independent observer confirmed write-after rollback: %s", table)
		})
	}
}

func TestKnowledgeWithdrawalMySQLExpiry(t *testing.T) {
	f := newPublicationFixture(t)
	end := publicationTime().Add(time.Hour)
	old, current := withdrawalPair(t, f, &end)
	oldBefore := f.state(t, old.ID)
	before := f.state(t, current.ID)

	for _, tc := range []struct {
		name    string
		at      time.Time
		expired bool
	}{
		{"before_1ms", end.Add(-time.Millisecond), false},
		{"exact_end", end, true},
		{"after_1ms", end.Add(time.Millisecond), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f.now.Store(tc.at.UnixNano())
			got, err := f.svc(f.db).GetCurrentByDocumentID(
				f.ctx, current.DocumentID,
			)
			if tc.expired {
				if got != nil ||
					!errors.Is(err, service.ErrKnowledgeCurrentVersionNotFound) {
					t.Fatalf("unexpected fallback=%+v err=%v", got, err)
				}
			} else {
				Must(t, err)
				if got == nil || got.Version == nil {
					t.Fatal("version unavailable before deadline")
				}
				Equal(t, got.Version.ID, current.ID)
			}

			history, err := f.svc(f.db).GetHistoryByVersionID(f.ctx, current.ID)
			Must(t, err)
			if history == nil {
				t.Fatal("expired history disappeared")
			}
			Equal(t, history.State, service.KnowledgeHistoryState{
				IsCurrent:          true,
				IsExpired:          tc.expired,
				IsCurrentEffective: !tc.expired,
			})
			Equal(t, history.Articles, before.Content.Articles)
			Equal(t, *history.Version, before.Content.Version)

			oldHistory, err := f.svc(f.db).GetHistoryByVersionID(f.ctx, old.ID)
			Must(t, err)
			if oldHistory == nil {
				t.Fatal("old history disappeared")
			}
			Equal(t, oldHistory.State,
				service.KnowledgeHistoryState{IsSuperseded: true})
			Equal(t, f.state(t, current.ID), before)
			Equal(t, f.state(t, old.ID), oldBefore)
		})
	}

	// 过期不阻止管理员清除仍然存在的当前选择。
	result, err := f.svc(f.db).WithdrawVersion(
		f.ctx, f.userID, withdrawalInput(current, 2),
	)
	Must(t, err)
	withdrawalWantCommitted(t, f, before, result)
	withdrawalWantNoCurrent(t, f, current.DocumentID)

	history, err := f.svc(f.db).GetHistoryByVersionID(f.ctx, current.ID)
	Must(t, err)
	if history == nil {
		t.Fatal("withdrawn expired history disappeared")
	}
	Equal(t, history.State, service.KnowledgeHistoryState{
		IsExpired: true, IsWithdrawn: true,
	})
	Equal(t, history.Articles, before.Content.Articles)
	switchWantFrozen(t, f.state(t, old.ID).Content, oldBefore.Content)
}

func TestKnowledgeWithdrawalMySQLConcurrency(t *testing.T) {
	for _, firstOp := range []string{"publish", "withdraw"} {
		t.Run(firstOp+"_first", func(t *testing.T) {
			f := newPublicationFixture(t)
			current := switchBaseline(t, f)
			candidate := switchNewRevision(t, f, current, "candidate", true)
			currentBefore := f.state(t, current.ID)
			candidateBefore := f.state(t, candidate.ID)

			secondOp := "withdraw"
			lockedVersionID := candidate.ID
			if firstOp == "withdraw" {
				secondOp = "publish"
				lockedVersionID = current.ID
			}

			locked := make(chan struct{})
			entering := make(chan struct{})
			releaseCh := make(chan struct{})
			var firstOnce, secondOnce, releaseOnce sync.Once
			var workers sync.WaitGroup
			release := func() {
				releaseOnce.Do(func() { close(releaseCh) })
			}
			defer func() {
				release()
				workers.Wait()
			}()

			firstName := f.prefix + "_withdraw_first"
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

			secondName := f.prefix + "_withdraw_second"
			Must(t, f.second.Callback().Query().Before("gorm:query").
				Register(secondName, func(tx *gorm.DB) {
					if tx.Statement.Schema == nil ||
						tx.Statement.Schema.Table != "knowledge_documents" {
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
				published *service.KnowledgePublicationResult
				withdrawn *service.KnowledgeWithdrawalResult
				err       error
			}
			start := func(db *gorm.DB, op string) <-chan outcome {
				done := make(chan outcome, 1)
				workers.Add(1)
				go func() {
					defer workers.Done()
					var result outcome
					if op == "publish" {
						result.published, result.err = f.svc(db).PublishVersion(
							f.ctx, f.userID, switchPublishInput(candidate),
						)
					} else {
						result.withdrawn, result.err = f.svc(db).WithdrawVersion(
							f.ctx, f.userID, withdrawalInput(current, 1),
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

			firstDone := start(f.db, firstOp)
			awaitSignal(locked)

			// 第一个操作已经锁住版本；独立连接确认文档和版本的真实锁。
			probe := func(value any, id uint64) {
				t.Helper()
				err := f.observer.WithContext(f.ctx).
					Transaction(func(tx *gorm.DB) error {
						return tx.Clauses(clause.Locking{
							Strength: "UPDATE", Options: "NOWAIT",
						}).Where("id = ?", id).First(value).Error
					})
				var mysqlErr *mysqlDriver.MySQLError
				if !errors.As(err, &mysqlErr) || mysqlErr.Number != 3572 {
					t.Fatalf("expected NOWAIT lock error 3572, got %v", err)
				}
				t.Logf("real NOWAIT lock confirmed: %T id=%d", value, id)
			}
			probe(&model.KnowledgeDocument{}, current.DocumentID)
			probe(&model.RuleVersion{}, lockedVersionID)

			secondDone := start(f.second, secondOp)
			awaitSignal(entering)
			release()

			first := awaitResult(firstDone)
			second := awaitResult(secondDone)
			workers.Wait()
			Must(t, first.err)
			if !errors.Is(second.err, service.ErrKnowledgePublicationConflict) ||
				second.published != nil || second.withdrawn != nil {
				t.Fatalf("second operation must conflict without result: %+v", second)
			}

			if firstOp == "publish" {
				switchWantPublished(t, f, current, candidate, first.published)
				switchWantFrozen(t,
					f.state(t, current.ID).Content, currentBefore.Content)
				effective, err := f.svc(f.observer).
					GetCurrentByDocumentID(f.ctx, current.DocumentID)
				Must(t, err)
				if effective == nil || effective.Version == nil {
					t.Fatal("winning publication is not readable")
				}
				Equal(t, effective.Version.ID, candidate.ID)
			} else {
				withdrawalWantCommitted(t, f, currentBefore, first.withdrawn)
				switchWantFrozen(t,
					f.state(t, candidate.ID).Content, candidateBefore.Content)
				withdrawalWantNoCurrent(t, f, current.DocumentID)
			}

			state := f.state(t, current.ID)
			Equal(t, state.Content.Document.PublicationRevision, uint64(2))
			Equal(t, len(state.Operations), 2)
			t.Logf("%s committed; %s rejected with publication conflict",
				firstOp, secondOp)
		})
	}
}
