package router

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
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

func publicationTime() time.Time {
	return time.Date(2026, 10, 27, 2, 0, 0, 123000000, time.UTC)
}

func publicationTimePtr(v time.Time) *time.Time {
	return &v
}

type publicationFixture struct {
	ctx      context.Context
	db       *gorm.DB
	second   *gorm.DB
	observer *gorm.DB
	userID   uint64
	category uint64
	prefix   string
	keys     []string
	now      atomic.Int64
	calls    atomic.Int64
}

type publicationDBState struct {
	Content    verificationDBState
	Operations []model.RuleOperation
}

func newPublicationFixture(t *testing.T) *publicationFixture {
	t.Helper()

	if os.Getenv("RUN_MYSQL_TESTS") != "1" ||
		os.Getenv("DB_NAME") != "bili_support_test" ||
		os.Getenv("TEST_ISOLATED") != "1" {
		t.Fatal("require RUN_MYSQL_TESTS=1, DB_NAME=bili_support_test, TEST_ISOLATED=1")
	}
	expectedUUID := os.Getenv("TEST_MYSQL_SERVER_UUID")
	if expectedUUID == "" {
		t.Fatal("read the preflight identity, then set TEST_MYSQL_SERVER_UUID explicitly")
	}

	cfg := config.Load()
	f := &publicationFixture{
		db:       OpenTestDB(t, cfg.Database),
		second:   OpenTestDB(t, cfg.Database),
		observer: OpenTestDB(t, cfg.Database),
	}
	f.now.Store(publicationTime().UnixNano())

	// 三个独立连接池各固定一个连接。
	// 所有身份检查在创建任何夹具之前完成。
	seen := make(map[uint64]bool)
	for _, db := range []*gorm.DB{f.db, f.second, f.observer} {
		pool, err := db.DB()
		Must(t, err)
		pool.SetMaxOpenConns(1)
		pool.SetMaxIdleConns(1)

		var identity struct {
			DatabaseName string
			ServerUUID   string
			ConnectionID uint64
		}
		Must(t, db.Raw(`
			SELECT DATABASE() AS database_name,
			       @@server_uuid AS server_uuid,
			       CONNECTION_ID() AS connection_id
		`).Scan(&identity).Error)

		if identity.DatabaseName != "bili_support_test" ||
			identity.ServerUUID != expectedUUID {
			t.Fatalf("unexpected database identity: %+v", identity)
		}
		if identity.ConnectionID == 0 || seen[identity.ConnectionID] {
			t.Fatalf("connections are not independent: %+v", identity)
		}
		seen[identity.ConnectionID] = true
		t.Logf("database=%s server_uuid=%s connection_id=%d",
			identity.DatabaseName, identity.ServerUUID, identity.ConnectionID)
	}

	initial := Counts(t, f.observer)
	var random [8]byte
	_, err := rand.Read(random[:])
	Must(t, err)
	f.prefix = "pub_" + hex.EncodeToString(random[:])

	// 在任何夹具写入前登记清理；只清理本次随机键及自有账号/分类。
	t.Cleanup(func() {
		var users, categories []uint64
		if f.userID != 0 {
			users = []uint64{f.userID}
		}
		if f.category != 0 {
			categories = []uint64{f.category}
		}
		if err := Cleanup(f.observer, f.keys, users, categories); err != nil {
			t.Errorf("fixture cleanup failed; preserve evidence: %v", err)
			return
		}
		Equal(t, Counts(t, f.observer), initial)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	f.ctx = ctx
	t.Cleanup(cancel)

	category := model.RuleCategory{
		Code: f.prefix, Name: "【虚构测试】发布分类",
	}
	Must(t, f.observer.Create(&category).Error)
	f.category = category.ID

	user := model.User{
		Username: f.prefix, PasswordHash: "test-only-not-a-login-password",
		Role: model.RoleAdmin,
	}
	Must(t, f.observer.Create(&user).Error)
	f.userID = user.ID
	return f
}

func (f *publicationFixture) clock() time.Time {
	f.calls.Add(1)
	return time.Unix(0, f.now.Load()).UTC()
}

func (f *publicationFixture) svc(db *gorm.DB) *service.KnowledgeService {
	return service.NewKnowledgeService(
		repository.NewKnowledgeRepositoryWithClock(db, f.clock),
	)
}

func (f *publicationFixture) draft(
	t *testing.T, effective, expires *time.Time, verified bool,
) *model.RuleVersion {
	t.Helper()
	key := fmt.Sprintf("%s_%d", f.prefix, len(f.keys)+1)
	f.keys = append(f.keys, key)

	v, err := f.svc(f.observer).CreateDraft(
		f.ctx, service.CreateKnowledgeDraftInput{
			DocumentKey: key, Version: key, CategoryID: f.category,
			Title: "【虚构测试】发布规则", Content: "【虚构测试】原文\n甲\n乙",
			SourceURL:   "https://www.bilibili.com/test-only",
			EffectiveAt: effective, ExpiresAt: expires,
			Articles: []service.CreateRuleArticleInput{
				{SectionPath: "测试章节", ArticleNumber: "1", Content: "甲"},
				{SectionPath: "测试章节", ArticleNumber: "2", Content: "乙"},
			},
		},
	)
	Must(t, err)
	if v == nil || v.ID == 0 || v.DocumentID == 0 {
		t.Fatal("invalid saved draft")
	}
	if verified {
		f.verify(t, v.ID, v.ContentRevision)
	}
	return v
}

func (f *publicationFixture) verify(t *testing.T, id, revision uint64) {
	t.Helper()
	record, err := f.svc(f.observer).VerifyDraft(
		f.ctx, f.userID, service.VerifyKnowledgeDraftInput{
			VersionID: id, ExpectedContentRevision: revision,
			VerificationNote: "【虚构测试】仅测试发布前置条件",
		},
	)
	Must(t, err)
	if record == nil || record.ID == 0 {
		t.Fatal("verification did not return a saved record")
	}
}

// 阶段04尚未提供同文档新增版本接口。
// 这里仅用自有测试夹具构造同文档第二版本，随后通过真实服务核验。
// 不代表新增版本业务已经实现或通过验收。
func (f *publicationFixture) sibling(
	t *testing.T, original *model.RuleVersion,
) *model.RuleVersion {
	t.Helper()
	v := f.draft(t, original.EffectiveAt, original.ExpiresAt, false)
	Must(t, f.observer.Model(&model.RuleVersion{}).
		Where("id = ?", v.ID).
		Update("document_id", original.DocumentID).Error)
	v.DocumentID = original.DocumentID
	f.verify(t, v.ID, v.ContentRevision)
	return v
}

func publicationInput(v *model.RuleVersion) service.PublishKnowledgeVersionInput {
	return service.PublishKnowledgeVersionInput{
		DocumentID: v.DocumentID, VersionID: v.ID,
		ExpectedPublicationRevision: 0,
	}
}

func publicationEdit(v *model.RuleVersion) service.EditKnowledgeDraftInput {
	return service.EditKnowledgeDraftInput{
		VersionID: v.ID, ExpectedContentRevision: v.ContentRevision,
		CategoryID: v.CategoryID, Title: "【虚构测试】编辑后的标题",
		Content:   "【虚构测试】编辑后的原文",
		SourceURL: v.SourceURL, EffectiveAt: v.EffectiveAt, ExpiresAt: v.ExpiresAt,
		Articles: []service.CreateRuleArticleInput{
			{SectionPath: "测试章节", ArticleNumber: "1", Content: "修改条款"},
		},
	}
}

func (f *publicationFixture) state(t *testing.T, id uint64) publicationDBState {
	t.Helper()
	s := publicationDBState{
		Content: readVerificationState(t, f.observer, id),
	}
	Must(t, f.observer.Where("document_id = ?", s.Content.Document.ID).
		Order("id ASC").Find(&s.Operations).Error)
	return s
}

func (f *publicationFixture) published(
	t *testing.T, v *model.RuleVersion,
	result *service.KnowledgePublicationResult,
) {
	t.Helper()
	if result == nil {
		t.Fatal("publication returned nil result")
	}
	s := f.state(t, v.ID)
	at := time.Unix(0, f.now.Load()).UTC().Truncate(time.Millisecond)

	Equal(t, result.DocumentID, v.DocumentID)
	Equal(t, result.VersionID, v.ID)
	Equal(t, result.PublicationRevision, uint64(1))
	Equal(t, result.PublishedAt.Equal(at), true)

	Equal(t, s.Content.Document.CurrentVersionID, &v.ID)
	Equal(t, s.Content.Document.PublicationRevision, uint64(1))
	Equal(t, s.Content.Version.Status, model.RuleVersionStatusPublished)
	if s.Content.Version.PublishedAt == nil ||
		!s.Content.Version.PublishedAt.Equal(at) {
		t.Fatal("saved publication time differs from operation time")
	}
	Equal(t, len(s.Operations), 1)
	op := s.Operations[0]
	Equal(t, op.DocumentID, v.DocumentID)
	Equal(t, op.VersionID, v.ID)
	Equal(t, op.OperationType, model.RuleOperationTypePublish)
	Equal(t, op.BeforeCurrentVersionID, (*uint64)(nil))
	Equal(t, op.AfterCurrentVersionID, &v.ID)
	Equal(t, op.BeforePublicationRevision, uint64(0))
	Equal(t, op.AfterPublicationRevision, uint64(1))
	Equal(t, op.OperatorID, f.userID)
	Equal(t, op.OperatedAt.Equal(at), true)

	if len(s.Content.Verifications) == 0 {
		t.Fatal("published without verification")
	}
	record := s.Content.Verifications[len(s.Content.Verifications)-1]
	Equal(t, record.VerifiedContentRevision, s.Content.Version.ContentRevision)
	var snapshot struct {
		VersionID       uint64 `json:"version_id"`
		ContentRevision uint64 `json:"content_revision"`
		Content         string `json:"content"`
	}
	Must(t, json.Unmarshal([]byte(record.ContentSnapshot), &snapshot))
	Equal(t, snapshot.VersionID, v.ID)
	Equal(t, snapshot.ContentRevision, s.Content.Version.ContentRevision)
	Equal(t, snapshot.Content, s.Content.Version.Content)
}

func TestKnowledgePublicationMySQLTimeAndFreeze(t *testing.T) {
	now := publicationTime()
	cases := []struct {
		name               string
		effective, expires *time.Time
		want               error
	}{
		{"unknown", nil, nil, service.ErrKnowledgeEffectiveTimeRequired},
		{"effective_after", publicationTimePtr(now.Add(time.Millisecond)), nil,
			service.ErrKnowledgeNotYetEffective},
		{"effective_equal", &now, nil, nil},
		{"effective_before", publicationTimePtr(now.Add(-time.Millisecond)), nil, nil},
		{"expiry_before", publicationTimePtr(now.Add(-time.Hour)),
			publicationTimePtr(now.Add(-time.Millisecond)), service.ErrKnowledgeExpired},
		{"expiry_equal", publicationTimePtr(now.Add(-time.Hour)), &now,
			service.ErrKnowledgeExpired},
		{"expiry_after", publicationTimePtr(now.Add(-time.Hour)),
			publicationTimePtr(now.Add(time.Millisecond)), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newPublicationFixture(t)
			v := f.draft(t, tc.effective, tc.expires, true)
			before := f.state(t, v.ID)

			result, err := f.svc(f.db).PublishVersion(f.ctx, f.userID, publicationInput(v))
			Equal(t, f.calls.Load(), int64(1))
			if tc.want != nil {
				if result != nil || !errors.Is(err, tc.want) {
					t.Fatalf("result=%+v error=%v want=%v", result, err, tc.want)
				}
				Equal(t, f.state(t, v.ID), before)
				return
			}
			Must(t, err)
			f.published(t, v, result)

			// 发布只修改管理状态，原文、条款、片段和核验记录不能改变。
			after := f.state(t, v.ID)
			Equal(t, after.Content.Version.Content, before.Content.Version.Content)
			Equal(t, after.Content.Articles, before.Content.Articles)
			Equal(t, after.Content.Chunks, before.Content.Chunks)
			Equal(t, after.Content.Verifications, before.Content.Verifications)

			for _, operation := range []string{"edit", "verify"} {
				var operationErr error
				switch operation {
				case "edit":
					got, e := f.svc(f.db).EditDraftWithVerification(f.ctx, publicationEdit(v))
					operationErr = e
					if got != nil {
						t.Fatal("published edit returned success")
					}
				case "verify":
					got, e := f.svc(f.db).VerifyDraft(f.ctx, f.userID,
						service.VerifyKnowledgeDraftInput{
							VersionID: v.ID, ExpectedContentRevision: 1,
							VerificationNote: "【虚构测试】发布后核验应拒绝",
						})
					operationErr = e
					if got != nil {
						t.Fatal("published verification returned success")
					}
				}
				if !errors.Is(operationErr, service.ErrKnowledgeVersionNotEditable) {
					t.Fatalf("%s error=%v", operation, operationErr)
				}
				Equal(t, f.state(t, v.ID), after)
			}

			// 当前查询同样检查起点及截止点。
			f.calls.Store(0)
			detail, err := f.svc(f.db).GetCurrentByDocumentID(f.ctx, v.DocumentID)
			Must(t, err)
			if detail == nil || detail.Version == nil {
				t.Fatal("nil current detail")
			}
			Equal(t, f.calls.Load(), int64(1))
			Equal(t, detail.Version.ID, v.ID)
			Equal(t, detail.Version.SourceURL, v.SourceURL)
			Equal(t, detail.Articles, before.Content.Articles)

			f.now.Store(tc.effective.Add(-time.Millisecond).UnixNano())
			detail, err = f.svc(f.db).GetCurrentByDocumentID(f.ctx, v.DocumentID)
			if detail != nil || !errors.Is(err, service.ErrKnowledgeCurrentVersionNotFound) {
				t.Fatalf("before effective: detail=%+v error=%v", detail, err)
			}
			if tc.expires != nil {
				for _, at := range []time.Time{*tc.expires, tc.expires.Add(time.Millisecond)} {
					f.now.Store(at.UnixNano())
					detail, err = f.svc(f.db).GetCurrentByDocumentID(f.ctx, v.DocumentID)
					if detail != nil || !errors.Is(err, service.ErrKnowledgeCurrentVersionNotFound) {
						t.Fatalf("expired: detail=%+v error=%v", detail, err)
					}
				}
			} else {
				f.now.Store(now.AddDate(20, 0, 0).UnixNano())
				_, err = f.svc(f.db).GetCurrentByDocumentID(f.ctx, v.DocumentID)
				Must(t, err)
			}
			// 查询不能偷偷改指针、修订号、状态或操作记录。
			Equal(t, f.state(t, v.ID), after)
		})
	}
}

func TestKnowledgePublicationMySQLRejections(t *testing.T) {
	cases := []struct {
		name string
		want error
	}{
		{"unverified", service.ErrKnowledgeVerificationRequired},
		{"stale_verification", service.ErrKnowledgeVerificationRequired},
		{"wrong_owner", service.ErrKnowledgeVersionNotFound},
		{"missing_version", service.ErrKnowledgeVersionNotFound},
		{"missing_document", service.ErrKnowledgeDocumentNotFound},
		{"stale_revision", service.ErrKnowledgePublicationConflict},
		{"revision_overflow", service.ErrKnowledgePublicationConflict},
		{"published", service.ErrKnowledgeVersionNotPublishable},
		{"withdrawn", service.ErrKnowledgeVersionNotPublishable},
		{"existing_pointer", service.ErrKnowledgeVersionNotPublishable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newPublicationFixture(t)
			at := publicationTime().Add(-time.Hour)
			v := f.draft(t, &at, nil, tc.name != "unverified")
			input := publicationInput(v)

			switch tc.name {
			case "stale_verification":
				_, err := f.svc(f.db).EditDraftWithVerification(f.ctx, publicationEdit(v))
				Must(t, err)
			case "wrong_owner":
				other := f.draft(t, &at, nil, true)
				input.VersionID = other.ID
			case "missing_version", "missing_document":
				var count int64
				if tc.name == "missing_version" {
					Must(t, f.observer.Model(&model.RuleVersion{}).
						Where("id = ?", ^uint64(0)).Count(&count).Error)
					input.VersionID = ^uint64(0)
				} else {
					Must(t, f.observer.Model(&model.KnowledgeDocument{}).
						Where("id = ?", ^uint64(0)).Count(&count).Error)
					input.DocumentID = ^uint64(0)
				}
				Equal(t, count, int64(0))
			case "stale_revision":
				input.ExpectedPublicationRevision = 1
			case "revision_overflow":
				Must(t, f.observer.Model(&model.KnowledgeDocument{}).
					Where("id = ?", v.DocumentID).
					Update("publication_revision", ^uint64(0)).Error)
				input.ExpectedPublicationRevision = ^uint64(0)
			case "published", "withdrawn":
				Must(t, f.observer.Model(&model.RuleVersion{}).
					Where("id = ?", v.ID).Update("status", tc.name).Error)
			case "existing_pointer":
				Must(t, f.observer.Model(&model.KnowledgeDocument{}).
					Where("id = ?", v.DocumentID).
					Update("current_version_id", v.ID).Error)
			}

			before := f.state(t, v.ID)
			counts := Counts(t, f.observer)
			result, err := f.svc(f.db).PublishVersion(f.ctx, f.userID, input)
			if result != nil || !errors.Is(err, tc.want) {
				t.Fatalf("result=%+v error=%v want=%v", result, err, tc.want)
			}
			Equal(t, f.state(t, v.ID), before)
			Equal(t, Counts(t, f.observer), counts)
		})
	}
}

func TestKnowledgePublicationMySQLRollback(t *testing.T) {
	for _, table := range []string{
		"rule_versions", "knowledge_documents", "rule_operations",
	} {
		t.Run(table, func(t *testing.T) {
			f := newPublicationFixture(t)
			at := publicationTime().Add(-time.Hour)
			v := f.draft(t, &at, nil, true)
			before := f.state(t, v.ID)
			counts := Counts(t, f.observer)
			failure := errors.New("controlled publication failure")
			injected := false
			name := f.prefix + "_fault"

			callback := func(tx *gorm.DB) {
				if tx.Error != nil || tx.Statement.Schema == nil ||
					tx.Statement.Schema.Table != table {
					return
				}
				// 在真实SQL成功执行后注入错误，验证此前写入会回滚。
				if tx.RowsAffected != 1 {
					tx.AddError(fmt.Errorf("expected one written row, got %d", tx.RowsAffected))
					return
				}
				injected = true
				tx.AddError(failure)
			}
			if table == "rule_operations" {
				Must(t, f.db.Callback().Create().After("gorm:create").Register(name, callback))
				t.Cleanup(func() { Must(t, f.db.Callback().Create().Remove(name)) })
			} else {
				Must(t, f.db.Callback().Update().After("gorm:update").Register(name, callback))
				t.Cleanup(func() { Must(t, f.db.Callback().Update().Remove(name)) })
			}

			result, err := f.svc(f.db).PublishVersion(f.ctx, f.userID, publicationInput(v))
			if !injected || result != nil || !errors.Is(err, failure) {
				t.Fatalf("injected=%v result=%+v error=%v", injected, result, err)
			}
			Equal(t, f.state(t, v.ID), before)
			Equal(t, Counts(t, f.observer), counts)
			t.Log("independent observer confirmed complete rollback after write to", table)
		})
	}

	t.Run("real_mysql_foreign_key_failure", func(t *testing.T) {
		f := newPublicationFixture(t)
		at := publicationTime().Add(-time.Hour)
		v := f.draft(t, &at, nil, true)
		var count int64
		Must(t, f.observer.Model(&model.User{}).
			Where("id = ?", ^uint64(0)).Count(&count).Error)
		Equal(t, count, int64(0))

		before := f.state(t, v.ID)
		result, err := f.svc(f.db).PublishVersion(f.ctx, ^uint64(0), publicationInput(v))
		var mysqlErr *mysqlDriver.MySQLError
		if result != nil || !errors.As(err, &mysqlErr) || mysqlErr.Number != 1452 {
			t.Fatalf("want actual FK error 1452: result=%+v error=%v", result, err)
		}
		Equal(t, f.state(t, v.ID), before)
	})

	t.Run("operation_revision_unique", func(t *testing.T) {
		f := newPublicationFixture(t)
		at := publicationTime().Add(-time.Hour)
		v := f.draft(t, &at, nil, true)
		result, err := f.svc(f.db).PublishVersion(f.ctx, f.userID, publicationInput(v))
		Must(t, err)
		f.published(t, v, result)
		before := f.state(t, v.ID)

		duplicate := before.Operations[0]
		duplicate.ID = 0
		err = f.observer.Omit(clause.Associations).Create(&duplicate).Error
		var mysqlErr *mysqlDriver.MySQLError
		if !errors.As(err, &mysqlErr) || mysqlErr.Number != 1062 {
			t.Fatalf("want unique constraint error 1062, got %v", err)
		}
		Equal(t, f.state(t, v.ID), before)
	})
}

func TestKnowledgePublicationMySQLCurrentSelection(t *testing.T) {
	for _, condition := range []string{
		"nil_pointer", "wrong_owner", "missing_target", "draft",
		"withdrawn", "withdrawn_at", "unknown_effective", "future", "expired",
	} {
		t.Run(condition, func(t *testing.T) {
			f := newPublicationFixture(t)
			at := publicationTime().Add(-time.Hour)
			v := f.draft(t, &at, nil, true)
			result, err := f.svc(f.db).PublishVersion(f.ctx, f.userID, publicationInput(v))
			Must(t, err)
			f.published(t, v, result)

			// 同文档还存在另一个有效published夹具。
			// 当前目标失效时仍必须返回无规则，不能挑这个版本兜底。
			alternative := f.sibling(t, v)
			Must(t, f.observer.Model(&model.RuleVersion{}).
				Where("id = ?", alternative.ID).
				Updates(map[string]any{
					"status": model.RuleVersionStatusPublished, "published_at": at,
				}).Error)

			switch condition {
			case "nil_pointer":
				Must(t, f.observer.Model(&model.KnowledgeDocument{}).
					Where("id = ?", v.DocumentID).Update("current_version_id", nil).Error)
			case "wrong_owner":
				other := f.draft(t, &at, nil, true)
				Must(t, f.observer.Model(&model.RuleVersion{}).
					Where("id = ?", other.ID).
					Update("status", model.RuleVersionStatusPublished).Error)
				Must(t, f.observer.Model(&model.KnowledgeDocument{}).
					Where("id = ?", v.DocumentID).
					Update("current_version_id", other.ID).Error)
			case "missing_target":
				var count int64
				Must(t, f.observer.Model(&model.RuleVersion{}).
					Where("id = ?", ^uint64(0)).Count(&count).Error)
				Equal(t, count, int64(0))
				Must(t, f.observer.Model(&model.KnowledgeDocument{}).
					Where("id = ?", v.DocumentID).
					Update("current_version_id", ^uint64(0)).Error)
			default:
				updates := map[string]any{}
				switch condition {
				case "draft", "withdrawn":
					updates["status"] = condition
				case "withdrawn_at":
					updates["withdrawn_at"] = publicationTime()
				case "unknown_effective":
					updates["effective_at"] = nil
				case "future":
					updates["effective_at"] = publicationTime().Add(time.Millisecond)
				case "expired":
					updates["expires_at"] = publicationTime()
				}
				Must(t, f.observer.Model(&model.RuleVersion{}).
					Where("id = ?", v.ID).Updates(updates).Error)
			}
			before := f.state(t, v.ID)
			detail, err := f.svc(f.db).GetCurrentByDocumentID(f.ctx, v.DocumentID)
			if detail != nil || !errors.Is(err, service.ErrKnowledgeCurrentVersionNotFound) {
				t.Fatalf("detail=%+v error=%v", detail, err)
			}
			Equal(t, f.state(t, v.ID), before)
		})
	}
}

func TestKnowledgePublicationMySQLCurrentSnapshot(t *testing.T) {
	f := newPublicationFixture(t)
	at := publicationTime().Add(-time.Hour)
	v := f.draft(t, &at, nil, true)
	result, err := f.svc(f.db).PublishVersion(f.ctx, f.userID, publicationInput(v))
	Must(t, err)
	f.published(t, v, result)
	old := f.state(t, v.ID)

	// 先让实际条款顺序与ID顺序不同，验证仓储确实按article_order排序。
	Must(t, f.observer.Model(&model.RuleArticle{}).
		Where("id = ?", old.Content.Articles[0].ID).Update("article_order", 20).Error)
	Must(t, f.observer.Model(&model.RuleArticle{}).
		Where("id = ?", old.Content.Articles[1].ID).Update("article_order", 10).Error)
	old = f.state(t, v.ID)

	name := f.prefix + "_snapshot"
	fired := false
	Must(t, f.db.Callback().Query().After("gorm:query").Register(name, func(tx *gorm.DB) {
		if fired || tx.Error != nil || tx.Statement.Schema == nil ||
			tx.Statement.Schema.Table != "rule_versions" {
			return
		}
		fired = true
		// 测试专用的人工交错，不是允许业务编辑已发布内容。
		// 另一个真实事务在读取版本之后、读取条款之前提交新内容。
		err := f.observer.WithContext(f.ctx).Transaction(func(w *gorm.DB) error {
			if err := w.Model(&model.RuleVersion{}).Where("id = ?", v.ID).
				Update("content", "【虚构测试】新原文").Error; err != nil {
				return err
			}
			if err := w.Model(&model.RuleArticle{}).Where("version_id = ?", v.ID).
				Update("content", "【虚构测试】新条款").Error; err != nil {
				return err
			}
			ids := []uint64{old.Content.Articles[0].ID, old.Content.Articles[1].ID}
			return w.Model(&model.KnowledgeChunk{}).Where("article_id IN ?", ids).
				Update("content", "【虚构测试】新条款").Error
		})
		if err != nil {
			tx.AddError(err)
		}
	}))
	defer func() { Must(t, f.db.Callback().Query().Remove(name)) }()

	detail, err := f.svc(f.db).GetCurrentByDocumentID(f.ctx, v.DocumentID)
	Must(t, err)
	if !fired || detail == nil || detail.Version == nil {
		t.Fatal("snapshot interleaving did not execute")
	}
	Equal(t, detail.Version.Content, old.Content.Version.Content)
	Equal(t, detail.Articles, old.Content.Articles)

	fresh, err := f.svc(f.second).GetCurrentByDocumentID(f.ctx, v.DocumentID)
	Must(t, err)
	Equal(t, fresh.Version.Content, "【虚构测试】新原文")
	for _, article := range fresh.Articles {
		Equal(t, article.Content, "【虚构测试】新条款")
	}
	t.Log("reader saw old version+articles; independent new reader saw committed new content")
}

func TestKnowledgePublicationMySQLConcurrency(t *testing.T) {
	for _, pair := range [][2]string{
		{"publish", "publish"},
		{"edit", "publish"},
		{"verify", "publish"},
		{"publish", "edit"},
		{"publish", "verify"},
		{"verify", "expiry"},
	} {
		t.Run(pair[0]+"_then_"+pair[1], func(t *testing.T) {
			runPublicationCompetition(t, pair[0], pair[1])
		})
	}
}

func runPublicationCompetition(t *testing.T, firstOp, secondOp string) {
	t.Helper()
	f := newPublicationFixture(t)
	at := publicationTime().Add(-time.Hour)
	end := publicationTime().Add(time.Second)
	// verify先行场景从未核验开始，验证发布确实读取到新提交的核验。
	v := f.draft(t, &at, &end, firstOp != "verify")
	secondVersion := v
	if firstOp == "publish" && secondOp == "publish" {
		secondVersion = f.sibling(t, v)
	}

	firstLocked := make(chan struct{})
	secondEntering := make(chan struct{})
	releaseFirst := make(chan struct{})
	var releaseOnce, firstOnce, secondOnce sync.Once
	var workers sync.WaitGroup
	release := func() { releaseOnce.Do(func() { close(releaseFirst) }) }
	defer func() {
		release()
		workers.Wait()
	}()

	firstName := f.prefix + "_first"
	Must(t, f.db.Callback().Query().After("gorm:query").Register(firstName, func(tx *gorm.DB) {
		if tx.Error != nil || tx.Statement.Schema == nil ||
			tx.Statement.Schema.Table != "rule_versions" {
			return
		}
		if _, exists := tx.Statement.Clauses["FOR"]; !exists {
			return
		}
		firstOnce.Do(func() {
			close(firstLocked)
			select {
			case <-releaseFirst:
			case <-f.ctx.Done():
				tx.AddError(f.ctx.Err())
			}
		})
	}))
	t.Cleanup(func() { Must(t, f.db.Callback().Query().Remove(firstName)) })

	secondTable := "rule_versions"
	if firstOp == "publish" && secondOp == "publish" {
		secondTable = "knowledge_documents"
	}
	secondName := f.prefix + "_second"
	Must(t, f.second.Callback().Query().Before("gorm:query").Register(secondName, func(tx *gorm.DB) {
		if tx.Statement.Schema == nil || tx.Statement.Schema.Table != secondTable {
			return
		}
		if _, exists := tx.Statement.Clauses["FOR"]; !exists {
			return
		}
		secondOnce.Do(func() { close(secondEntering) })
	}))
	t.Cleanup(func() { Must(t, f.second.Callback().Query().Remove(secondName)) })

	type outcome struct {
		publication *service.KnowledgePublicationResult
		edit        *service.KnowledgeDraftEditResult
		verify      *model.RuleVerification
		err         error
	}
	start := func(db *gorm.DB, operation string, target *model.RuleVersion) <-chan outcome {
		done := make(chan outcome, 1)
		workers.Add(1)
		go func() {
			defer workers.Done()
			var result outcome
			svc := f.svc(db)
			switch operation {
			case "publish", "expiry":
				result.publication, result.err = svc.PublishVersion(
					f.ctx, f.userID, publicationInput(target))
			case "edit":
				result.edit, result.err = svc.EditDraftWithVerification(
					f.ctx, publicationEdit(target))
			case "verify":
				result.verify, result.err = svc.VerifyDraft(
					f.ctx, f.userID, service.VerifyKnowledgeDraftInput{
						VersionID: target.ID, ExpectedContentRevision: 1,
						VerificationNote: "【虚构测试】竞争中的核验",
					})
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

	firstDone := start(f.db, firstOp, v)
	awaitSignal(firstLocked)

	// 真实MySQL NOWAIT探针证明行锁存在，不以回调触发本身当锁证据。
	probe := func(value any, id uint64) {
		t.Helper()
		err := f.observer.WithContext(f.ctx).Transaction(func(tx *gorm.DB) error {
			return tx.Clauses(clause.Locking{
				Strength: "UPDATE", Options: "NOWAIT",
			}).Where("id = ?", id).First(value).Error
		})
		var mysqlErr *mysqlDriver.MySQLError
		if !errors.As(err, &mysqlErr) || mysqlErr.Number != 3572 {
			t.Fatalf("expected NOWAIT conflict 3572, got %v", err)
		}
	}
	probe(&model.RuleVersion{}, v.ID)
	if firstOp == "publish" {
		probe(&model.KnowledgeDocument{}, v.DocumentID)
	}

	secondDone := start(f.second, secondOp, secondVersion)
	awaitSignal(secondEntering)

	if secondOp == "expiry" {
		// 第二个发布操作已经到达版本锁读取入口。
		// 在第一事务释放锁前跨过失效终点，发布必须按新时间拒绝。
		Equal(t, f.calls.Load(), int64(0))
		f.now.Store(end.UnixNano())
	}
	release()

	first := awaitResult(firstDone)
	second := awaitResult(secondDone)
	workers.Wait()
	Must(t, first.err)

	var expected error
	switch {
	case firstOp == "publish" && secondOp == "publish":
		expected = service.ErrKnowledgePublicationConflict
	case firstOp == "edit":
		expected = service.ErrKnowledgeVerificationRequired
	case secondOp == "expiry":
		expected = service.ErrKnowledgeExpired
	case firstOp == "publish":
		expected = service.ErrKnowledgeVersionNotEditable
	}

	if expected == nil {
		Must(t, second.err)
	} else {
		if !errors.Is(second.err, expected) ||
			second.publication != nil || second.edit != nil || second.verify != nil {
			t.Fatalf("second result=%+v want error=%v", second, expected)
		}
	}

	state := f.state(t, v.ID)
	switch firstOp {
	case "publish":
		f.published(t, v, first.publication)
		Equal(t, state.Content.Version.ContentRevision, uint64(1))
		Equal(t, len(state.Content.Verifications), 1)
		if secondVersion.ID != v.ID {
			loser := f.state(t, secondVersion.ID)
			Equal(t, loser.Content.Version.Status, model.RuleVersionStatusDraft)
			Equal(t, loser.Content.Version.PublishedAt, (*time.Time)(nil))
		}
	case "edit":
		if first.edit == nil || first.edit.Version == nil {
			t.Fatal("missing first edit result")
		}
		Equal(t, state.Content.Version.ContentRevision, uint64(2))
		Equal(t, state.Content.Version.Status, model.RuleVersionStatusDraft)
		Equal(t, state.Content.Document.CurrentVersionID, (*uint64)(nil))
		Equal(t, state.Content.Document.PublicationRevision, uint64(0))
		Equal(t, len(state.Operations), 0)
		Equal(t, len(state.Content.Articles), 1)
		Equal(t, len(state.Content.Chunks), 1)
		Equal(t, state.Content.Chunks[0].ArticleID, state.Content.Articles[0].ID)
		Equal(t, state.Content.Chunks[0].Content, "修改条款")
		Equal(t, state.Content.Verifications[0].VerifiedContentRevision, uint64(1))
	case "verify":
		if first.verify == nil {
			t.Fatal("missing first verification result")
		}
		Equal(t, first.verify.VerifiedContentRevision, uint64(1))
		Equal(t, len(state.Content.Verifications), 1)
		if secondOp == "expiry" {
			Equal(t, state.Content.Version.Status, model.RuleVersionStatusDraft)
			Equal(t, state.Content.Document.CurrentVersionID, (*uint64)(nil))
			Equal(t, state.Content.Document.PublicationRevision, uint64(0))
			Equal(t, len(state.Operations), 0)
			Equal(t, f.calls.Load(), int64(1))
		} else {
			f.published(t, v, second.publication)
		}
	}
	t.Logf("real lock confirmed; first=%s second=%s outcome verified", firstOp, secondOp)
}
