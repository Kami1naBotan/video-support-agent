package router

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"video-support-agent/internal/auth"
	"video-support-agent/internal/config"
	"video-support-agent/internal/model"
	"video-support-agent/internal/service"

	"go.uber.org/zap"
)

func TestKnowledgeWithdrawalActualRouterMySQL(t *testing.T) {
	f := newPublicationFixture(t)

	tokens, err := auth.NewTokenManager(
		strings.Repeat("w", 32),
		"knowledge-withdrawal-actual-router-test",
		time.Hour,
	)
	Must(t, err)
	adminToken, err := tokens.Issue(f.userID)
	Must(t, err)

	roleTokens := map[string]string{model.RoleAdmin: adminToken}
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

	r := New(
		config.Config{AppName: "knowledge-withdrawal-router-test"},
		f.db, tokens, nil, AllowLimiter{}, nil, zap.NewNop(), nil,
	)

	// 实际Router使用系统时钟；以宽裕的过去时间准备版本。
	// 精确过期边界由上一板块的注入时钟测试负责。
	now := time.Now().UTC().Truncate(time.Millisecond)
	f.now.Store(now.UnixNano())
	effective := now.Add(-24 * time.Hour)
	old := f.draft(t, &effective, nil, true)
	first, err := f.svc(f.observer).PublishVersion(
		f.ctx, f.userID, publicationInput(old),
	)
	Must(t, err)
	f.published(t, old, first)

	current := switchNewRevision(t, f, old, "http-withdraw-current", true)
	second, err := f.svc(f.observer).PublishVersion(
		f.ctx, f.userID, switchPublishInput(current),
	)
	Must(t, err)
	switchWantPublished(t, f, old, current, second)

	// 另一份自有文档用于验证错误归属。
	other := f.draft(t, &effective, nil, false)
	tracked := []uint64{old.ID, current.ID, other.ID}
	oldBefore := f.state(t, old.ID)
	before := f.state(t, current.ID)

	documentPath := fmt.Sprintf(
		"/api/v1/admin/knowledge/documents/%d", old.DocumentID,
	)
	withdrawPath := documentPath + "/withdraw"
	currentPath := fmt.Sprintf(
		"/api/v1/knowledge/documents/%d/current", old.DocumentID,
	)
	historyPath := fmt.Sprintf(
		"/api/v1/knowledge/versions/%d", current.ID,
	)
	articlePath := fmt.Sprintf(
		"%s/articles/%d", historyPath, before.Content.Articles[0].ID,
	)
	body := func(versionID, revision uint64) string {
		return JSON(t, map[string]any{
			"version_id":                    versionID,
			"expected_publication_revision": revision,
		})
	}
	validBody := body(current.ID, 2)

	// 每个拒绝请求都必须保持所有自有版本及知识库记录计数不变。
	reject := func(
		method, path, payload, token string, status int, code string,
	) {
		t.Helper()
		states := make([]publicationDBState, len(tracked))
		for i, id := range tracked {
			states[i] = f.state(t, id)
		}
		counts := Counts(t, f.observer)
		w := Request(r, method, path, payload, token, false)
		WantError(t, w, status, code)
		if status == 401 {
			Equal(t, w.Header().Get("WWW-Authenticate"), "Bearer")
		}
		for i, id := range tracked {
			Equal(t, f.state(t, id), states[i])
		}
		Equal(t, Counts(t, f.observer), counts)
	}

	reject("POST", withdrawPath, validBody, "", 401, "unauthorized")
	reject("POST", withdrawPath, validBody, "invalid-token", 401, "unauthorized")
	for _, role := range []string{model.RoleUser, model.RoleSupport} {
		reject("POST", withdrawPath, validBody, roleTokens[role], 403, "forbidden")
		reject("GET", documentPath, "", roleTokens[role], 403, "forbidden")
	}
	for _, path := range []string{currentPath, historyPath, articlePath} {
		reject("GET", path, "", "", 401, "unauthorized")
	}

	// 同一JWT下修改数据库角色，验证权限来自当前数据库状态。
	Must(t, f.observer.Model(&model.User{}).Where("id = ?", f.userID).
		Update("role", model.RoleUser).Error)
	reject("POST", withdrawPath, validBody, adminToken, 403, "forbidden")
	Must(t, f.observer.Model(&model.User{}).Where("id = ?", f.userID).
		Update("role", model.RoleAdmin).Error)

	reject("POST", withdrawPath,
		JSON(t, map[string]any{
			"version_id": current.ID,
		}), adminToken, 400, "invalid_argument")
	reject("POST", withdrawPath,
		JSON(t, map[string]any{
			"version_id": current.ID, "expected_publication_revision": 2,
			"operator_id": extraUsers[0],
		}), adminToken, 400, "invalid_argument")
	reject("POST", withdrawPath, strings.Repeat(" ", 65537),
		adminToken, 413, "request_too_large")
	reject("POST", withdrawPath, body(other.ID, 2),
		adminToken, 404, "knowledge_version_not_found")
	reject("POST", withdrawPath, body(old.ID, 2),
		adminToken, 409, "knowledge_version_not_withdrawable")
	reject("POST", withdrawPath, body(current.ID, 1),
		adminToken, 409, "knowledge_publication_conflict")

	w := Request(r, "GET", documentPath, "", adminToken, false)
	WantJSON(t, w, 200, map[string]any{
		"document_id":          old.DocumentID,
		"current_version_id":   current.ID,
		"publication_revision": 2,
	})

	// 下线前，普通用户能通过实际路由读到新版。
	w = Request(r, "GET", currentPath, "", roleTokens[model.RoleUser], false)
	if w.Code != 200 {
		t.Fatalf("before withdrawal status=%d body=%s", w.Code, w.Body.String())
	}
	var currentResponse struct {
		VersionID uint64 `json:"version_id"`
	}
	Must(t, json.Unmarshal(w.Body.Bytes(), &currentResponse))
	Equal(t, currentResponse.VersionID, current.ID)

	// 成功下线必须经过真实HTTP入口。
	started := time.Now().UTC().Truncate(time.Millisecond)
	w = Request(r, "POST", withdrawPath, validBody, adminToken, false)
	finished := time.Now().UTC().Truncate(time.Millisecond)
	if w.Code != 200 {
		t.Fatalf("withdraw status=%d body=%s", w.Code, w.Body.String())
	}
	var response struct {
		DocumentID          uint64    `json:"document_id"`
		VersionID           uint64    `json:"version_id"`
		PublicationRevision uint64    `json:"publication_revision"`
		WithdrawnAt         time.Time `json:"withdrawn_at"`
	}
	Must(t, json.Unmarshal(w.Body.Bytes(), &response))
	if response.WithdrawnAt.IsZero() ||
		response.WithdrawnAt.Before(started) ||
		response.WithdrawnAt.After(finished) {
		t.Fatalf("withdrawal timestamp outside request interval: %+v", response)
	}
	WantJSON(t, w, 200, map[string]any{
		"document_id":          old.DocumentID,
		"version_id":           current.ID,
		"publication_revision": 3,
		"withdrawn_at":         response.WithdrawnAt,
	})

	// 将夹具断言时钟对齐已验证的实际响应时间，不改变Router时钟。
	// 共用断言检查响应、版本时间、文档时间及操作时间完全一致。
	f.now.Store(response.WithdrawnAt.UnixNano())
	withdrawalWantCommitted(t, f, before, &service.KnowledgeWithdrawalResult{
		DocumentID:          response.DocumentID,
		VersionID:           response.VersionID,
		PublicationRevision: response.PublicationRevision,
		WithdrawnAt:         response.WithdrawnAt,
	})
	switchWantFrozen(t, f.state(t, old.ID).Content, oldBefore.Content)
	after := f.state(t, current.ID)
	oldAfter := f.state(t, old.ID)

	w = Request(r, "GET", documentPath, "", adminToken, false)
	WantJSON(t, w, 200, map[string]any{
		"document_id":          old.DocumentID,
		"current_version_id":   nil,
		"publication_revision": 3,
	})
	reject("POST", withdrawPath, validBody,
		adminToken, 409, "knowledge_publication_conflict")
	reject("POST", withdrawPath, body(current.ID, 3),
		adminToken, 409, "knowledge_version_not_withdrawable")
	reject("POST", documentPath+"/publish", body(old.ID, 3),
		adminToken, 409, "knowledge_version_not_publishable")

	// 根据下线前数据库基线构造公开历史期望，不从响应反推期望。
	checkHistory := func(
		baseline publicationDBState, superseded, withdrawn bool, token string,
	) {
		t.Helper()
		v := baseline.Content.Version
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
		articles := make([]map[string]any, len(baseline.Content.Articles))
		for i, a := range baseline.Content.Articles {
			articles[i] = map[string]any{
				"article_id": a.ID, "article_order": a.ArticleOrder,
				"section_path": a.SectionPath, "article_number": a.ArticleNumber,
				"content": a.Content,
			}
		}
		state := map[string]any{
			"is_current": false, "is_superseded": superseded,
			"is_expired": false, "is_withdrawn": withdrawn,
			"is_current_effective": false,
		}
		path := fmt.Sprintf("/api/v1/knowledge/versions/%d", v.ID)
		got := Request(r, "GET", path, "", token, false)
		WantJSON(t, got, 200, map[string]any{
			"version": versionJSON, "content": v.Content,
			"articles": articles, "state": state,
		})
		for i, a := range baseline.Content.Articles {
			got = Request(r, "GET",
				fmt.Sprintf("%s/articles/%d", path, a.ID), "", token, false)
			WantJSON(t, got, 200, map[string]any{
				"version": versionJSON, "article": articles[i], "state": state,
			})
		}
	}

	for _, role := range []string{
		model.RoleUser, model.RoleSupport, model.RoleAdmin,
	} {
		token := roleTokens[role]
		reject("GET", currentPath, "", token,
			404, "knowledge_current_version_not_found")
		checkHistory(oldBefore, true, false, token)
		checkHistory(before, false, true, token)
	}

	// 读取和被拒绝的请求不追加操作、不修改历史。
	Equal(t, f.state(t, current.ID), after)
	Equal(t, f.state(t, old.ID), oldAfter)
	t.Log("actual router verified withdrawal permissions, conflicts, no fallback and public history")
}
