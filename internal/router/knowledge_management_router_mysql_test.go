package router

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"video-support-agent/internal/auth"
	"video-support-agent/internal/config"
	"video-support-agent/internal/model"
)

func TestKnowledgeManagementActualRouterMySQL(t *testing.T) {
	if os.Getenv("RUN_MYSQL_TESTS") != "1" ||
		os.Getenv("DB_NAME") != "bili_support_test" ||
		os.Getenv("TEST_ISOLATED") != "1" {
		t.Fatal("require RUN_MYSQL_TESTS=1, DB_NAME=bili_support_test, TEST_ISOLATED=1")
	}

	cfg := config.Load()
	db := OpenTestDB(t, cfg.Database)
	observer := OpenTestDB(t, cfg.Database)
	initial := Counts(t, observer)

	var random [12]byte
	_, err := rand.Read(random[:])
	Must(t, err)
	key := "test-management-http-" + hex.EncodeToString(random[:])

	var users, categories []uint64
	t.Cleanup(func() {
		if err := Cleanup(observer, []string{key}, users, categories); err != nil {
			t.Errorf("cleanup failed: %v; do not delete unrelated data", err)
			return
		}
		Equal(t, Counts(t, observer), initial)
	})

	category := model.RuleCategory{Code: key, Name: "虚构HTTP测试分类"}
	Must(t, db.Create(&category).Error)
	categories = append(categories, category.ID)

	tokens, err := auth.NewTokenManager(
		strings.Repeat("k", 32), "knowledge-management-test", time.Hour,
	)
	Must(t, err)

	roleTokens := map[string]string{}
	roleIDs := map[string]uint64{}
	for _, role := range []string{
		model.RoleAdmin, model.RoleUser, model.RoleSupport,
	} {
		user := model.User{
			Username:     key + "-" + role,
			PasswordHash: "test-fixture-not-a-login-password",
			Role:         role,
		}
		Must(t, db.Create(&user).Error)
		users = append(users, user.ID)
		roleIDs[role] = user.ID

		token, err := tokens.Issue(user.ID)
		Must(t, err)
		roleTokens[role] = token
	}

	// 使用真实路由组装，仅限流使用既有测试替身。
	// 不启动网络监听，不连接Redis或LLM。
	r := New(
		config.Config{AppName: "knowledge-management-test"},
		db, tokens, nil, AllowLimiter{}, nil, nil, nil,
	)
	base := "/api/v1/admin/knowledge"
	adminToken := roleTokens[model.RoleAdmin]

	input := Input()
	input.DocumentKey = key
	input.CategoryID = category.ID
	input.SourceURL = "https://www.bilibili.com/test-only"
	input.Title = "【虚构测试】实际管理路由"

	w := Request(r, "POST", base+"/documents",
		JSON(t, Payload(input)), adminToken, false)
	if w.Code != 201 {
		t.Fatalf("create status=%d body=%s", w.Code, w.Body.String())
	}
	var created struct {
		DocumentID uint64 `json:"document_id"`
		VersionID  uint64 `json:"version_id"`
	}
	Must(t, json.Unmarshal(w.Body.Bytes(), &created))
	if created.DocumentID == 0 || created.VersionID == 0 {
		t.Fatalf("invalid IDs: %+v", created)
	}

	path := fmt.Sprintf("%s/versions/%d", base, created.VersionID)
	verifyPath := path + "/verifications"

	// 使用原始JSON保留null与对象的区别。
	readSummary := func(
		wantRevision uint64, wantVerificationID uint64,
		wantVerifiedRevision uint64, wantMatch bool,
	) {
		t.Helper()
		response := Request(r, "GET", path, "", adminToken, false)
		if response.Code != 200 {
			t.Fatalf("read status=%d body=%s",
				response.Code, response.Body.String())
		}

		var detail struct {
			VersionID       uint64          `json:"version_id"`
			ContentRevision uint64          `json:"content_revision"`
			Verification    json.RawMessage `json:"verification"`
		}
		Must(t, json.Unmarshal(response.Body.Bytes(), &detail))
		Equal(t, detail.VersionID, created.VersionID)
		Equal(t, detail.ContentRevision, wantRevision)

		if wantVerificationID == 0 {
			Equal(t, string(detail.Verification), "null")
			return
		}

		var summary struct {
			ID       uint64 `json:"verification_id"`
			Revision uint64 `json:"verified_content_revision"`
			UserID   uint64 `json:"verified_by"`
			Matches  bool   `json:"matches_current_content"`
		}
		Must(t, json.Unmarshal(detail.Verification, &summary))
		Equal(t, summary.ID, wantVerificationID)
		Equal(t, summary.Revision, wantVerifiedRevision)
		Equal(t, summary.UserID, roleIDs[model.RoleAdmin])
		Equal(t, summary.Matches, wantMatch)

		var fields map[string]json.RawMessage
		Must(t, json.Unmarshal(detail.Verification, &fields))
		Equal(t, len(fields), 7)
		for _, forbidden := range []string{
			"content_snapshot", "snapshot_schema_version", "verifier", "version",
		} {
			if _, exists := fields[forbidden]; exists {
				t.Fatalf("internal field leaked: %s", forbidden)
			}
		}
	}

	readSummary(1, 0, 0, false)

	verifyBody := func(revision uint64) string {
		return JSON(t, map[string]any{
			"expected_content_revision": revision,
			"verification_note":         "【虚构测试】仅验证HTTP核验流程",
			"evidence_url":              nil,
		})
	}

	editPayload := Payload(input)
	delete(editPayload, "document_key")
	delete(editPayload, "version")
	editPayload["expected_content_revision"] = 1
	editPayload["content"] = "【虚构测试】编辑后的全文"
	editPayload["articles"] = []map[string]any{
		{
			"section_path":   "虚构章节",
			"article_number": "1",
			"content":        "【虚构测试】编辑后的条款",
		},
	}
	editBody := JSON(t, editPayload)

	// 拒绝访问后，不仅检查状态码，还检查数据库没有变化。
	beforeDenied := readVerificationState(t, observer, created.VersionID)
	for _, role := range []string{"anonymous", model.RoleUser, model.RoleSupport} {
		token, status, code := "", 401, "unauthorized"
		if role != "anonymous" {
			token, status, code = roleTokens[role], 403, "forbidden"
		}
		for _, request := range []struct {
			method, path, body string
		}{
			{"GET", path, ""},
			{"PUT", path, editBody},
			{"POST", verifyPath, verifyBody(1)},
		} {
			response := Request(r, request.method, request.path,
				request.body, token, false)
			WantError(t, response, status, code)
		}
		Equal(t, readVerificationState(t, observer, created.VersionID), beforeDenied)
	}

	// 首次核验：请求中的伪造查询参数不能覆盖JWT身份。
	w = Request(r, "POST", verifyPath+"?verified_by=999&role=admin",
		verifyBody(1), adminToken, false)
	if w.Code != 201 {
		t.Fatalf("verify status=%d body=%s", w.Code, w.Body.String())
	}
	var verified struct {
		ID       uint64 `json:"verification_id"`
		UserID   uint64 `json:"verified_by"`
		Revision uint64 `json:"verified_content_revision"`
	}
	Must(t, json.Unmarshal(w.Body.Bytes(), &verified))
	if verified.ID == 0 {
		t.Fatal("missing verification ID")
	}
	Equal(t, verified.UserID, roleIDs[model.RoleAdmin])
	Equal(t, verified.Revision, uint64(1))
	readSummary(1, verified.ID, 1, true)

	state := readVerificationState(t, observer, created.VersionID)
	Equal(t, len(state.Verifications), 1)
	Equal(t, state.Verifications[0].VerifiedBy, roleIDs[model.RoleAdmin])
	firstRecord := state.Verifications[0]

	// 编辑后内容修订号递增，旧核验保留但不再匹配。
	w = Request(r, "PUT", path, editBody, adminToken, false)
	if w.Code != 200 {
		t.Fatalf("edit status=%d body=%s", w.Code, w.Body.String())
	}
	var edited struct {
		VersionID uint64 `json:"version_id"`
		Revision  uint64 `json:"content_revision"`
		Summary   struct {
			ID      uint64 `json:"verification_id"`
			Matches bool   `json:"matches_current_content"`
		} `json:"verification"`
	}
	Must(t, json.Unmarshal(w.Body.Bytes(), &edited))
	Equal(t, edited.VersionID, created.VersionID)
	Equal(t, edited.Revision, uint64(2))
	Equal(t, edited.Summary.ID, verified.ID)
	Equal(t, edited.Summary.Matches, false)
	readSummary(2, verified.ID, 1, false)

	beforeConflict := readVerificationState(t, observer, created.VersionID)
	w = Request(r, "POST", verifyPath, verifyBody(1), adminToken, false)
	WantError(t, w, 409, "knowledge_content_conflict")
	w = Request(r, "PUT", path, editBody, adminToken, false)
	WantError(t, w, 409, "knowledge_content_conflict")
	Equal(t, readVerificationState(t, observer, created.VersionID), beforeConflict)

	// 对当前修订重新核验，旧记录及快照不能被覆盖。
	w = Request(r, "POST", verifyPath, verifyBody(2), adminToken, false)
	if w.Code != 201 {
		t.Fatalf("reverify status=%d body=%s", w.Code, w.Body.String())
	}
	var second struct {
		ID uint64 `json:"verification_id"`
	}
	Must(t, json.Unmarshal(w.Body.Bytes(), &second))
	if second.ID <= verified.ID {
		t.Fatal("reverification did not append a new record")
	}
	readSummary(2, second.ID, 2, true)
	state = readVerificationState(t, observer, created.VersionID)
	Equal(t, len(state.Verifications), 2)
	Equal(t, state.Verifications[0], firstRecord)

	// 使用数据库确认不存在的ID，不能仅凭数字很大推断不存在。
	missingID := ^uint64(0) - 1
	for _, table := range []string{"rule_versions", "rule_categories"} {
		var count int64
		Must(t, observer.Table(table).
			Where("id = ?", missingID).Count(&count).Error)
		if count != 0 {
			t.Fatalf("missing-ID fixture is occupied in %s", table)
		}
	}

	// 此时当前内容修订号为2。
	validEdit := Payload(input)
	delete(validEdit, "document_key")
	delete(validEdit, "version")
	validEdit["expected_content_revision"] = 2

	assertRejectedRequest := func(
		method, requestPath, body string,
		status int, code string, unknownLength bool,
	) {
		t.Helper()
		before := readVerificationState(t, observer, created.VersionID)
		countsBefore := Counts(t, observer)

		response := Request(r, method, requestPath, body,
			adminToken, unknownLength)
		WantError(t, response, status, code)

		Equal(t, readVerificationState(t, observer, created.VersionID), before)
		Equal(t, Counts(t, observer), countsBefore)
	}

	missingPath := fmt.Sprintf("%s/versions/%d", base, missingID)
	assertRejectedRequest(
		"GET", missingPath, "",
		404, "knowledge_draft_not_found", false,
	)
	assertRejectedRequest(
		"PUT", missingPath, JSON(t, validEdit),
		404, "knowledge_version_not_found", false,
	)
	assertRejectedRequest(
		"POST", missingPath+"/verifications", verifyBody(2),
		404, "knowledge_version_not_found", false,
	)

	validEdit["category_id"] = missingID
	assertRejectedRequest(
		"PUT", path, JSON(t, validEdit),
		404, "rule_category_not_found", false,
	)
	validEdit["category_id"] = category.ID

	// 验证真实路由对已发布/已下线版本的行为。
	// 这里只设置测试状态，不代表发布和下线功能已经实现。
	for _, status := range []model.RuleVersionStatus{
		model.RuleVersionStatusPublished,
		model.RuleVersionStatusWithdrawn,
	} {
		update := db.Model(&model.RuleVersion{}).
			Where("id = ?", created.VersionID).
			Update("status", status)
		Must(t, update.Error)
		Equal(t, update.RowsAffected, int64(1))

		assertRejectedRequest(
			"GET", path, "",
			404, "knowledge_draft_not_found", false,
		)
		assertRejectedRequest(
			"PUT", path, JSON(t, validEdit),
			409, "knowledge_version_not_editable", false,
		)
		assertRejectedRequest(
			"POST", verifyPath, verifyBody(2),
			409, "knowledge_version_not_editable", false,
		)
	}

	update := db.Model(&model.RuleVersion{}).
		Where("id = ?", created.VersionID).
		Update("status", model.RuleVersionStatusDraft)
	Must(t, update.Error)
	Equal(t, update.RowsAffected, int64(1))

	// 已知长度和未知长度都覆盖，避免只测试Content-Length预检查。
	largeEdit := JSON(t, validEdit)
	largeEdit += strings.Repeat(" ", 65537-len(largeEdit))
	largeVerify := verifyBody(2)
	largeVerify += strings.Repeat(" ", 65537-len(largeVerify))

	for _, unknownLength := range []bool{false, true} {
		assertRejectedRequest(
			"PUT", path, largeEdit,
			413, "request_too_large", unknownLength,
		)
		assertRejectedRequest(
			"POST", verifyPath, largeVerify,
			413, "request_too_large", unknownLength,
		)
	}

	// 同一个JWT仍有效，但数据库角色已变，管理员权限必须立即失效。
	Must(t, db.Model(&model.User{}).
		Where("id = ?", roleIDs[model.RoleAdmin]).
		Update("role", model.RoleUser).Error)

	beforeDemotionRequests := readVerificationState(t, observer, created.VersionID)
	editPayload["expected_content_revision"] = 2
	for _, request := range []struct {
		method, path, body string
	}{
		{"GET", path, ""},
		{"PUT", path, JSON(t, editPayload)},
		{"POST", verifyPath, verifyBody(2)},
	} {
		w = Request(r, request.method, request.path,
			request.body, adminToken, false)
		WantError(t, w, 403, "forbidden")
	}
	Equal(t, readVerificationState(t, observer, created.VersionID),
		beforeDemotionRequests)

	t.Log("actual router: admin create/read/edit/verify/reverify succeeded; stale requests=409; anonymous=401; user/support/demoted admin=403; rejected writes left state unchanged")
}
