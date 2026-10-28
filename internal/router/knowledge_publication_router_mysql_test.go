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

func TestKnowledgePublicationActualRouterMySQL(t *testing.T) {
	f := newPublicationFixture(t)

	tokens, err := auth.NewTokenManager(
		strings.Repeat("p", 32), "publication-actual-router-test", time.Hour,
	)
	Must(t, err)

	adminToken, err := tokens.Issue(f.userID)
	Must(t, err)

	roleTokens := map[string]string{
		model.RoleAdmin: adminToken,
	}
	roleIDs := map[string]uint64{
		model.RoleAdmin: f.userID,
	}

	var extraUsers []uint64
	t.Cleanup(func() {
		if err := Cleanup(f.observer, nil, extraUsers, nil); err != nil {
			t.Errorf("extra role fixture cleanup failed: %v", err)
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
		roleIDs[role] = user.ID

		token, err := tokens.Issue(user.ID)
		Must(t, err)
		roleTokens[role] = token
	}

	// 真实应用组装：JWT、数据库角色、Handler、Service、Repository。
	// 只替换限流，不连接Redis、LLM，也不发送网络请求。
	r := New(
		config.Config{AppName: "publication-actual-router-test"},
		f.db, tokens, nil, AllowLimiter{}, nil, zap.NewNop(), nil,
	)

	status := func(w *httptest.ResponseRecorder, expected int) {
		t.Helper()
		if w.Code != expected {
			t.Fatalf("status=%d want=%d body=%s",
				w.Code, expected, w.Body.String())
		}
	}

	base := "/api/v1/admin/knowledge"
	key := f.prefix + "_http"
	f.keys = append(f.keys, key)

	// 实际router使用系统时钟；这里使用宽裕的过去时间，
	// 毫秒等值边界已经由板块②的注入时钟验证。
	effective := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Millisecond)
	input := Input()
	input.DocumentKey = key
	input.CategoryID = f.category
	input.Title = "【虚构测试】真实发布HTTP链路"
	input.Content = "【虚构测试】发布原文"
	input.SourceURL = "https://www.bilibili.com/test-only"
	input.EffectiveAt = &effective
	input.ExpiresAt = nil

	w := Request(r, "POST", base+"/documents",
		JSON(t, Payload(input)), adminToken, false)
	status(w, 201)

	var created struct {
		DocumentID uint64 `json:"document_id"`
		VersionID  uint64 `json:"version_id"`
	}
	Must(t, json.Unmarshal(w.Body.Bytes(), &created))
	if created.DocumentID == 0 || created.VersionID == 0 {
		t.Fatalf("invalid created IDs: %+v", created)
	}

	documentPath := fmt.Sprintf("%s/documents/%d", base, created.DocumentID)
	publishPath := documentPath + "/publish"
	versionPath := fmt.Sprintf("%s/versions/%d", base, created.VersionID)
	currentPath := fmt.Sprintf(
		"/api/v1/knowledge/documents/%d/current", created.DocumentID,
	)

	publishBody := func(versionID, revision uint64) string {
		return JSON(t, map[string]any{
			"version_id":                    versionID,
			"expected_publication_revision": revision,
		})
	}
	validBody := publishBody(created.VersionID, 0)

	// 每次拒绝请求后，既比较本版本完整状态，也比较全知识库表计数。
	rejected := func(
		method, path, body, token string,
		expectedStatus int, code string, unknownLength bool,
	) {
		t.Helper()
		before := f.state(t, created.VersionID)
		counts := Counts(t, f.observer)
		response := Request(r, method, path, body, token, unknownLength)
		WantError(t, response, expectedStatus, code)
		if expectedStatus == 401 {
			Equal(t, response.Header().Get("WWW-Authenticate"), "Bearer")
		}
		Equal(t, f.state(t, created.VersionID), before)
		Equal(t, Counts(t, f.observer), counts)
	}

	// 管理详情在未发布时也能提供publication_revision。
	w = Request(r, "GET", documentPath, "", adminToken, false)
	WantJSON(t, w, 200, map[string]any{
		"document_id":          created.DocumentID,
		"current_version_id":   nil,
		"publication_revision": 0,
	})

	// 真实路由必须先鉴权，不能让普通用户进入发布服务。
	for _, role := range []string{
		"anonymous", model.RoleUser, model.RoleSupport,
	} {
		token, expectedStatus, code := "", 401, "unauthorized"
		if role != "anonymous" {
			token = roleTokens[role]
			expectedStatus, code = 403, "forbidden"
		}
		rejected("POST", publishPath, validBody, token,
			expectedStatus, code, false)
		rejected("GET", documentPath, "", token,
			expectedStatus, code, false)
	}

	rejected("GET", currentPath, "", "", 401, "unauthorized", false)
	rejected("GET", currentPath, "", "invalid-token", 401, "unauthorized", false)

	// 三种登录角色都能进入查询，但未发布时没有当前规则。
	for _, role := range []string{
		model.RoleUser, model.RoleSupport, model.RoleAdmin,
	} {
		rejected("GET", currentPath, "", roleTokens[role],
			404, "knowledge_current_version_not_found", false)
	}

	// 未核验不能发布。
	rejected("POST", publishPath, validBody, adminToken,
		409, "knowledge_verification_required", false)

	// 通过真实核验HTTP入口准备发布，不直接插入核验记录。
	verifyBody := JSON(t, map[string]any{
		"expected_content_revision": 1,
		"verification_note":         "【虚构测试】仅验证真实HTTP发布链路",
		"evidence_url":              nil,
	})
	w = Request(r, "POST", versionPath+"/verifications",
		verifyBody, adminToken, false)
	status(w, 201)

	beforePublish := f.state(t, created.VersionID)
	Equal(t, len(beforePublish.Content.Verifications), 1)
	Equal(t, beforePublish.Content.Verifications[0].VerifiedBy, f.userID)

	// 确认输入错误和请求体限制在实际路由上仍然生效。
	for _, body := range []string{
		"",
		"null",
		"{",
		validBody + "{}",
		fmt.Sprintf(`{"version_id":%d}`, created.VersionID),
		fmt.Sprintf(
			`{"version_id":%d,"expected_publication_revision":null}`,
			created.VersionID,
		),
		strings.Replace(validBody, "{", `{"operator_id":999,`, 1),
	} {
		rejected("POST", publishPath, body, adminToken,
			400, "invalid_argument", false)
	}

	for _, id := range []string{"0", "-1", "abc", "18446744073709551616"} {
		rejected("POST", base+"/documents/"+id+"/publish", validBody,
			adminToken, 400, "invalid_argument", false)
		rejected("GET", "/api/v1/knowledge/documents/"+id+"/current", "",
			roleTokens[model.RoleUser], 400, "invalid_argument", false)
	}

	oversized := validBody + strings.Repeat(" ", 65537-len(validBody))
	for _, unknownLength := range []bool{false, true} {
		rejected("POST", publishPath, oversized, adminToken,
			413, "request_too_large", unknownLength)
	}

	// 精确64KiB、未知Content-Length请求应成功。
	// 查询参数伪造操作者不能覆盖JWT身份。
	exact := validBody + strings.Repeat(" ", 65536-len(validBody))
	started := time.Now().UTC().Truncate(time.Millisecond)
	w = Request(r, "POST", publishPath+"?operator_id=999&role=user",
		exact, adminToken, true)
	finished := time.Now().UTC()
	status(w, 200)

	var published struct {
		DocumentID          uint64    `json:"document_id"`
		VersionID           uint64    `json:"version_id"`
		PublicationRevision uint64    `json:"publication_revision"`
		PublishedAt         time.Time `json:"published_at"`
	}
	Must(t, json.Unmarshal(w.Body.Bytes(), &published))
	Equal(t, published.DocumentID, created.DocumentID)
	Equal(t, published.VersionID, created.VersionID)
	Equal(t, published.PublicationRevision, uint64(1))
	if published.PublishedAt.Before(started) || published.PublishedAt.After(finished) {
		t.Fatalf("publication time outside request interval: %v", published.PublishedAt)
	}
	WantJSON(t, w, 200, map[string]any{
		"document_id":          created.DocumentID,
		"version_id":           created.VersionID,
		"publication_revision": 1,
		"published_at":         published.PublishedAt,
	})

	saved := f.state(t, created.VersionID)
	Equal(t, saved.Content.Document.CurrentVersionID, &created.VersionID)
	Equal(t, saved.Content.Document.PublicationRevision, uint64(1))
	Equal(t, saved.Content.Version.Status, model.RuleVersionStatusPublished)
	if saved.Content.Version.PublishedAt == nil ||
		!saved.Content.Version.PublishedAt.Equal(published.PublishedAt) {
		t.Fatal("HTTP result and saved version publication time differ")
	}
	Equal(t, saved.Content.Articles, beforePublish.Content.Articles)
	Equal(t, saved.Content.Chunks, beforePublish.Content.Chunks)
	Equal(t, saved.Content.Verifications, beforePublish.Content.Verifications)
	Equal(t, len(saved.Operations), 1)

	operation := saved.Operations[0]
	Equal(t, operation.DocumentID, created.DocumentID)
	Equal(t, operation.VersionID, created.VersionID)
	Equal(t, operation.OperatorID, f.userID)
	Equal(t, operation.OperationType, model.RuleOperationTypePublish)
	Equal(t, operation.BeforeCurrentVersionID, (*uint64)(nil))
	Equal(t, operation.AfterCurrentVersionID, &created.VersionID)
	Equal(t, operation.BeforePublicationRevision, uint64(0))
	Equal(t, operation.AfterPublicationRevision, uint64(1))
	Equal(t, operation.OperatedAt.Equal(published.PublishedAt), true)

	w = Request(r, "GET", documentPath, "", adminToken, false)
	WantJSON(t, w, 200, map[string]any{
		"document_id":          created.DocumentID,
		"current_version_id":   created.VersionID,
		"publication_revision": 1,
	})

	// 精确比较整个公开响应，额外字段同样导致失败。
	articles := make([]map[string]any, len(saved.Content.Articles))
	for i, article := range saved.Content.Articles {
		articles[i] = map[string]any{
			"article_id":     article.ID,
			"article_order":  article.ArticleOrder,
			"section_path":   article.SectionPath,
			"article_number": article.ArticleNumber,
			"content":        article.Content,
		}
	}
	version := saved.Content.Version
	publicResponse := map[string]any{
		"document_id":  version.DocumentID,
		"version_id":   version.ID,
		"version":      version.Version,
		"category_id":  version.CategoryID,
		"title":        version.Title,
		"content":      version.Content,
		"source_url":   version.SourceURL,
		"effective_at": version.EffectiveAt,
		"expires_at":   version.ExpiresAt,
		"articles":     articles,
	}
	for _, role := range []string{
		model.RoleUser, model.RoleSupport, model.RoleAdmin,
	} {
		w = Request(r, "GET", currentPath, "", roleTokens[role], false)
		WantJSON(t, w, 200, publicResponse)
	}
	Equal(t, f.state(t, created.VersionID), saved)

	// 同一操作重试不能新增管理记录。
	rejected("POST", publishPath, validBody, adminToken,
		409, "knowledge_publication_conflict", false)
	rejected("POST", publishPath, publishBody(created.VersionID, 1), adminToken,
		409, "knowledge_version_not_publishable", false)

	// 真实发布之后，编辑与再次核验也必须拒绝。
	editPayload := Payload(input)
	delete(editPayload, "document_key")
	delete(editPayload, "version")
	editPayload["expected_content_revision"] = 1
	editPayload["content"] = "【虚构测试】不应写入的覆盖内容"

	rejected("PUT", versionPath, JSON(t, editPayload), adminToken,
		409, "knowledge_version_not_editable", false)
	rejected("POST", versionPath+"/verifications", verifyBody, adminToken,
		409, "knowledge_version_not_editable", false)

	// JWT未过期，但数据库管理员角色被撤销：管理权限立即失效。
	Must(t, f.observer.Model(&model.User{}).
		Where("id = ?", f.userID).Update("role", model.RoleUser).Error)

	rejected("POST", publishPath, publishBody(created.VersionID, 1), adminToken,
		403, "forbidden", false)
	rejected("GET", documentPath, "", adminToken, 403, "forbidden", false)

	// 降为普通用户后，仍可查询公开规则。
	w = Request(r, "GET", currentPath, "", adminToken, false)
	WantJSON(t, w, 200, publicResponse)

	// 已删除账号即使仍持有合法JWT，也不能访问当前规则。
	Must(t, f.observer.Delete(&model.User{}, roleIDs[model.RoleSupport]).Error)
	rejected("GET", currentPath, "", roleTokens[model.RoleSupport],
		401, "unauthorized", false)

	t.Log("actual router verified create -> verify -> publish -> current query")
	t.Log("verified 400/401/403/404/409/413, exact public DTO, operator identity and role revocation")
}

func TestKnowledgePublicationActualRouterRejectionsMySQL(t *testing.T) {
	cases := []struct {
		name string
		code string
	}{
		{"unknown", "knowledge_effective_time_required"},
		{"future", "knowledge_not_yet_effective"},
		{"expired", "knowledge_expired"},
		{"unverified", "knowledge_verification_required"},
		{"stale_verification", "knowledge_verification_required"},
		{"wrong_owner", "knowledge_version_not_found"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newPublicationFixture(t)
			now := time.Now().UTC().Truncate(time.Millisecond)
			effective := publicationTimePtr(now.Add(-24 * time.Hour))
			var expires *time.Time

			switch tc.name {
			case "unknown":
				effective = nil
			case "future":
				effective = publicationTimePtr(now.Add(24 * time.Hour))
			case "expired":
				expires = publicationTimePtr(now.Add(-time.Hour))
			}

			v := f.draft(t, effective, expires, tc.name != "unverified")
			targetID := v.ID

			switch tc.name {
			case "stale_verification":
				_, err := f.svc(f.observer).EditDraftWithVerification(
					f.ctx, publicationEdit(v))
				Must(t, err)
			case "wrong_owner":
				other := f.draft(t, effective, expires, true)
				targetID = other.ID
			}

			tokens, err := auth.NewTokenManager(
				strings.Repeat("r", 32), "publication-rejections-test", time.Hour,
			)
			Must(t, err)
			token, err := tokens.Issue(f.userID)
			Must(t, err)

			r := New(
				config.Config{AppName: "publication-rejections-test"},
				f.db, tokens, nil, AllowLimiter{}, nil, zap.NewNop(), nil,
			)

			before := f.state(t, v.ID)
			counts := Counts(t, f.observer)
			body := JSON(t, map[string]any{
				"version_id":                    targetID,
				"expected_publication_revision": 0,
			})
			path := fmt.Sprintf(
				"/api/v1/admin/knowledge/documents/%d/publish", v.DocumentID,
			)
			expectedStatus := 409
			if tc.name == "wrong_owner" {
				expectedStatus = 404
			}

			w := Request(r, "POST", path, body, token, false)
			WantError(t, w, expectedStatus, tc.code)

			// 发布被拒绝后，管理状态仍能读取，当前查询仍没有规则。
			statePath := fmt.Sprintf(
				"/api/v1/admin/knowledge/documents/%d", v.DocumentID,
			)
			w = Request(r, "GET", statePath, "", token, false)
			WantJSON(t, w, 200, map[string]any{
				"document_id":          v.DocumentID,
				"current_version_id":   nil,
				"publication_revision": 0,
			})

			currentPath := fmt.Sprintf(
				"/api/v1/knowledge/documents/%d/current", v.DocumentID,
			)
			w = Request(r, "GET", currentPath, "", token, false)
			WantError(t, w, 404, "knowledge_current_version_not_found")

			Equal(t, f.state(t, v.ID), before)
			Equal(t, Counts(t, f.observer), counts)
		})
	}
}
