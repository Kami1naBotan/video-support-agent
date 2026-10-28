package router

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"video-support-agent/internal/auth"
	"video-support-agent/internal/config"
	"video-support-agent/internal/model"

	"go.uber.org/zap"
)

func TestKnowledgeSwitchActualRouterMySQL(t *testing.T) {
	f := newPublicationFixture(t)

	tokens, err := auth.NewTokenManager(
		strings.Repeat("s", 32),
		"knowledge-switch-actual-router-test",
		time.Hour,
	)
	Must(t, err)

	adminToken, err := tokens.Issue(f.userID)
	Must(t, err)
	roleTokens := map[string]string{
		model.RoleAdmin: adminToken,
	}

	var extraUsers []uint64
	t.Cleanup(func() {
		if err := Cleanup(f.observer, nil, extraUsers, nil); err != nil {
			t.Errorf("role fixture cleanup failed: %v", err)
		}
	})
	for _, role := range []string{model.RoleUser, model.RoleSupport} {
		user := model.User{
			Username:     f.prefix + "_" + role,
			PasswordHash: "test-only-not-a-login-password",
			Role:         role,
		}
		Must(t, f.observer.Create(&user).Error)
		extraUsers = append(extraUsers, user.ID)
		token, err := tokens.Issue(user.ID)
		Must(t, err)
		roleTokens[role] = token
	}

	// 真实路由组装；仅限流使用替身。
	r := New(
		config.Config{AppName: "knowledge-switch-router-test"},
		f.db, tokens, nil, AllowLimiter{}, nil, zap.NewNop(), nil,
	)

	// 实际router使用系统时钟；精确时间边界由受控时钟测试负责。
	now := time.Now().UTC().Truncate(time.Millisecond)
	f.now.Store(now.UnixNano())
	effective := now.Add(-24 * time.Hour)
	old := f.draft(t, &effective, nil, true)
	published, err := f.svc(f.observer).PublishVersion(
		f.ctx, f.userID, publicationInput(old),
	)
	Must(t, err)
	f.published(t, old, published)
	oldBefore := f.state(t, old.ID)

	documentPath := fmt.Sprintf(
		"/api/v1/admin/knowledge/documents/%d", old.DocumentID,
	)
	createPath := documentPath + "/versions"
	publishPath := documentPath + "/publish"
	oldHistoryPath := fmt.Sprintf(
		"/api/v1/knowledge/versions/%d", old.ID,
	)
	oldArticle := oldBefore.Content.Articles[0]
	oldArticlePath := fmt.Sprintf(
		"%s/articles/%d", oldHistoryPath, oldArticle.ID,
	)

	payload := map[string]any{
		"version":             "http-next",
		"revision_type":       "editorial",
		"revision_note":       "【虚构测试】HTTP录入纠错",
		"revision_source_url": nil,
		"category_id":         f.category,
		"title":               "【虚构测试】HTTP新版",
		"content":             "【虚构测试】新版全文",
		"source_url":          "https://www.bilibili.com/test-only",
		"effective_at":        effective,
		"expires_at":          nil,
		"articles": []map[string]any{
			{
				"section_path":   "测试章节",
				"article_number": "1",
				"content":        "【虚构测试】新版条款",
			},
		},
	}
	body := JSON(t, payload)

	status := func(w *httptest.ResponseRecorder, want int) {
		t.Helper()
		if w.Code != want {
			t.Fatalf("status=%d want=%d body=%s",
				w.Code, want, w.Body.String())
		}
	}

	// 每次拒绝均比较自有版本状态及全知识库表计数。
	trackedIDs := []uint64{old.ID}
	reject := func(method, path, requestBody, token string, want int, code string) {
		t.Helper()
		before := make([]publicationDBState, len(trackedIDs))
		for i, id := range trackedIDs {
			before[i] = f.state(t, id)
		}
		counts := Counts(t, f.observer)

		w := Request(r, method, path, requestBody, token, false)
		WantError(t, w, want, code)

		for i, id := range trackedIDs {
			Equal(t, f.state(t, id), before[i])
		}
		Equal(t, Counts(t, f.observer), counts)
	}

	// 未登录不能创建修订或访问历史。
	reject("POST", createPath, body, "", 401, "unauthorized")
	reject("GET", oldHistoryPath, "", "", 401, "unauthorized")
	reject("GET", oldArticlePath, "", "", 401, "unauthorized")

	for _, role := range []string{model.RoleUser, model.RoleSupport} {
		reject("POST", createPath, body, roleTokens[role], 403, "forbidden")
		reject("GET", documentPath, "", roleTokens[role], 403, "forbidden")
	}

	// 管理员获取发布修订号，作为随后发布请求的依据。
	w := Request(r, "GET", documentPath, "", adminToken, false)
	WantJSON(t, w, 200, map[string]any{
		"document_id":          old.DocumentID,
		"current_version_id":   old.ID,
		"publication_revision": 1,
	})

	// 新版必须通过实际HTTP入口创建。
	w = Request(r, "POST", createPath, body, adminToken, false)
	status(w, 201)
	var created struct {
		DocumentID uint64 `json:"document_id"`
		VersionID  uint64 `json:"version_id"`
	}
	Must(t, json.Unmarshal(w.Body.Bytes(), &created))
	if created.VersionID == 0 || created.VersionID == old.ID {
		t.Fatal("invalid new version identity")
	}
	WantJSON(t, w, 201, map[string]any{
		"document_id": old.DocumentID,
		"version_id":  created.VersionID,
		"version":     "http-next",
		"status":      "draft",
	})
	trackedIDs = append(trackedIDs, created.VersionID)
	next := f.state(t, created.VersionID)
	Equal(t, next.Content.Document, oldBefore.Content.Document)
	Equal(t, len(next.Content.Verifications), 0)
	Equal(t, len(next.Content.Articles), 1)
	Equal(t, f.state(t, old.ID), oldBefore)

	draftPath := fmt.Sprintf(
		"/api/v1/admin/knowledge/versions/%d", created.VersionID,
	)
	nextHistoryPath := fmt.Sprintf(
		"/api/v1/knowledge/versions/%d", created.VersionID,
	)
	nextArticlePath := fmt.Sprintf(
		"%s/articles/%d", nextHistoryPath, next.Content.Articles[0].ID,
	)
	verifyPath := draftPath + "/verifications"
	verifyBody := JSON(t, map[string]any{
		"expected_content_revision": 1,
		"verification_note":         "【虚构测试】HTTP来源核验",
		"evidence_url":              nil,
	})
	publishBody := JSON(t, map[string]any{
		"version_id":                    created.VersionID,
		"expected_publication_revision": 1,
	})

	// 实际路由权限：管理操作始终需要管理员身份。
	for _, role := range []string{model.RoleUser, model.RoleSupport} {
		token := roleTokens[role]
		reject("GET", draftPath, "", token, 403, "forbidden")
		reject("POST", verifyPath, verifyBody, token, 403, "forbidden")
		reject("POST", publishPath, publishBody, token, 403, "forbidden")
	}
	reject("POST", createPath, body, adminToken, 409, "knowledge_version_exists")

	// 三类角色均不能通过公开历史路由读取草稿。
	for _, role := range []string{model.RoleUser, model.RoleSupport, model.RoleAdmin} {
		token := roleTokens[role]
		reject("GET", nextHistoryPath, "", token, 404, "knowledge_version_not_found")
		reject("GET", nextArticlePath, "", token, 404, "knowledge_version_not_found")
	}

	// 认证核验人必须来自JWT对应的实际数据库用户。
	w = Request(r, "POST", verifyPath, verifyBody, adminToken, false)
	status(w, 201)
	verified := f.state(t, created.VersionID)
	Equal(t, len(verified.Content.Verifications), 1)
	Equal(t, verified.Content.Verifications[0].VerifiedBy, f.userID)
	Equal(t, verified.Content.Verifications[0].VerifiedContentRevision, uint64(1))

	// 发布前还不能作为历史公开读取。
	reject("GET", nextHistoryPath, "", adminToken, 404, "knowledge_version_not_found")

	w = Request(r, "POST", publishPath, publishBody, adminToken, false)
	status(w, 200)
	var result struct {
		DocumentID          uint64    `json:"document_id"`
		VersionID           uint64    `json:"version_id"`
		PublicationRevision uint64    `json:"publication_revision"`
		PublishedAt         time.Time `json:"published_at"`
	}
	Must(t, json.Unmarshal(w.Body.Bytes(), &result))
	Equal(t, result.DocumentID, old.DocumentID)
	Equal(t, result.VersionID, created.VersionID)
	Equal(t, result.PublicationRevision, uint64(2))
	if result.PublishedAt.IsZero() {
		t.Fatal("publication response has no timestamp")
	}

	saved := f.state(t, created.VersionID)
	Equal(t, saved.Content.Document.CurrentVersionID, &created.VersionID)
	Equal(t, saved.Content.Document.PublicationRevision, uint64(2))
	Equal(t, len(saved.Operations), 2)
	last := saved.Operations[1]
	Equal(t, last.BeforeCurrentVersionID, &old.ID)
	Equal(t, last.AfterCurrentVersionID, &created.VersionID)
	Equal(t, last.BeforePublicationRevision, uint64(1))
	Equal(t, last.AfterPublicationRevision, uint64(2))
	Equal(t, last.OperatorID, f.userID)
	switchWantFrozen(t, f.state(t, old.ID).Content, oldBefore.Content)

	// 重试旧请求会因预期修订号过时而返回409，不能追加管理记录。
	reject("POST", publishPath, publishBody, adminToken,
		409, "knowledge_publication_conflict")
	w = Request(r, "GET", documentPath, "", adminToken, false)
	WantJSON(t, w, 200, map[string]any{
		"document_id":          old.DocumentID,
		"current_version_id":   created.VersionID,
		"publication_revision": 2,
	})

	// 完整JSON比较保证普通响应没有内部核验身份、修订依据或关联模型。
	checkHistory := func(versionID uint64, current, superseded bool, token string) {
		t.Helper()
		s := f.state(t, versionID)
		v := s.Content.Version
		versionJSON := map[string]any{
			"document_id":   v.DocumentID,
			"version_id":    v.ID,
			"version":       v.Version,
			"category_id":   v.CategoryID,
			"revision_type": string(v.RevisionType),
			"title":         v.Title,
			"source_url":    v.SourceURL,
			"effective_at":  v.EffectiveAt,
			"expires_at":    v.ExpiresAt,
			"published_at":  v.PublishedAt,
		}
		articles := make([]map[string]any, len(s.Content.Articles))
		for i, a := range s.Content.Articles {
			articles[i] = map[string]any{
				"article_id": a.ID, "article_order": a.ArticleOrder,
				"section_path": a.SectionPath, "article_number": a.ArticleNumber,
				"content": a.Content,
			}
		}
		state := map[string]any{
			"is_current": current, "is_superseded": superseded,
			"is_expired": false, "is_withdrawn": false,
			"is_current_effective": current,
		}
		path := fmt.Sprintf("/api/v1/knowledge/versions/%d", versionID)
		response := Request(r, "GET", path, "", token, false)
		WantJSON(t, response, 200, map[string]any{
			"version": versionJSON, "content": v.Content,
			"articles": articles, "state": state,
		})
		path += fmt.Sprintf("/articles/%d", s.Content.Articles[0].ID)
		response = Request(r, "GET", path, "", token, false)
		WantJSON(t, response, 200, map[string]any{
			"version": versionJSON, "article": articles[0], "state": state,
		})
	}

	for _, role := range []string{model.RoleUser, model.RoleSupport, model.RoleAdmin} {
		token := roleTokens[role]
		checkHistory(old.ID, false, true, token)
		checkHistory(created.VersionID, true, false, token)

		// 即便条款ID真实存在，也不能跨版本串读。
		wrongPath := fmt.Sprintf("%s/articles/%d", nextHistoryPath, oldArticle.ID)
		reject("GET", wrongPath, "", token, 404, "knowledge_article_not_found")
		reject("GET", "/api/v1/knowledge/versions/0", "", token, 400, "invalid_argument")
	}

	// 历史查询和拒绝请求不能修改任何已发布数据。
	Equal(t, f.state(t, created.VersionID), saved)
	switchWantFrozen(t, f.state(t, old.ID).Content, oldBefore.Content)
}
