//go:build searchintegration && searchliveintegration

package searchintegration

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"video-support-agent/internal/auth"
	"video-support-agent/internal/cache"
	"video-support-agent/internal/config"
	"video-support-agent/internal/elasticsearch"
	indexing "video-support-agent/internal/index"
	"video-support-agent/internal/model"
	"video-support-agent/internal/repository"
	"video-support-agent/internal/retrieval"
	"video-support-agent/internal/router"
)

func TestSearchLive(t *testing.T) {
	// 先创建MySQL夹具，再注册ES保护检查。
	// Cleanup按逆序执行：先检查ES，再决定是否删除测试库。
	s := newSearchSuite(t)
	e := newLiveEnvironment(t)

	suffix := strings.TrimPrefix(s.name, "es_search_it_")
	alias := "rule_chunks_search_it_" + suffix
	index := "rule_chunks_v1_" +
		time.Now().UTC().Format("20060102t150405z") + "_" + suffix

	t.Logf("TEST_ALIAS=%s TEST_INDEX=%s", alias, index)
	e.request(t, "HEAD", "/"+index, nil, 404)
	e.request(t, "GET", "/_alias/"+alias, nil, 404)

	esClient := liveClient(t, liveESEndpoint, alias)
	queryAt := s.at
	repo := repository.NewKnowledgeRepositoryWithClock(
		s.db, func() time.Time { return s.at },
	)
	search, err := retrieval.NewServiceWithClock(
		esClient, repo, func() time.Time { return queryAt },
	)
	must(t, err)

	// 测试内容仅存在于自建MySQL库和独立ES索引。
	chinese := liveFixture(t, s, "测试规则", "投稿审核", "第十条",
		"<script>alert(1)</script> 退款申请 税 API接口")

	// 对照语料：目标词各出现一次，分别只出现在标题和正文。
	titleHit := liveFixture(t, s, "ranktoken", "controlled", "A", "neutral")
	bodyHit := liveFixture(t, s, "neutral", "controlled", "B", "ranktoken")

	effective := liveFixture(t, s, "边界测试", "时间", "C", "boundarytoken")
	expiring := liveFixture(t, s, "边界测试", "时间", "D", "boundarytoken")
	must(t, s.db.Model(&model.RuleVersion{}).
		Where("id = ?", effective.Chunk.VersionID).
		Update("effective_at", s.at).Error)
	must(t, s.db.Model(&model.RuleVersion{}).
		Where("id = ?", expiring.Chunk.VersionID).
		Update("expires_at", s.at.Add(time.Millisecond)).Error)

	stateDocs := make([]retrieval.Candidate, 3)
	for i := range stateDocs {
		stateDocs[i] = liveFixture(t, s, "状态测试", "状态", "E", "statefiltertoken")
	}
	staleDocs := make(map[uint64]retrieval.Candidate)
	for i := 0; i < 6; i++ {
		c := liveFixture(t, s, "陈旧测试", "候选", "F", "staletoken")
		staleDocs[c.Chunk.ChunkID] = c
	}

	runCase(t, "real_export_rebuild_and_alias", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		result, err := indexing.Rebuild(ctx, repo, esClient, indexing.RebuildOptions{
			NewIndex:           index,
			Definition:         e.definition,
			ExpectedDatabase:   s.name,
			ExpectedServerUUID: os.Getenv("INDEX_EXPORT_EXPECT_SERVER_UUID"),
		})
		must(t, err)
		if result == nil || result.Stage != "complete" ||
			result.DocumentCount != 14 ||
			result.Publication.State != indexing.PublicationApplied ||
			result.Publication.ObservedTarget != index {
			t.Fatalf("重建结果不符：%+v", result)
		}
		target, err := esClient.GetAliasTarget(ctx)
		must(t, err)
		if target != index || e.count(t, index) != 14 {
			t.Fatal("测试Alias或实际文档计数不符")
		}
		var health struct {
			Status   string `json:"status"`
			TimedOut bool   `json:"timed_out"`
		}
		must(t, json.Unmarshal(e.request(t, "GET",
			"/_cluster/health/"+index+"?wait_for_status=green&timeout=10s",
			nil, 200), &health))
		if health.Status != "green" || health.TimedOut {
			t.Fatal("测试索引未恢复green")
		}
		t.Logf("REBUILD_COUNT=14 ALIAS_TARGET=%s", target)
	})

	retrieve := func(t *testing.T, query string, opts retrieval.SearchOptions) []retrieval.RuleChunk {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		got, err := search.Retrieve(ctx, query, opts)
		must(t, err)
		return got
	}

	for _, tc := range []struct {
		name  string
		query string
		opts  retrieval.SearchOptions
		want  []uint64
	}{
		{"chinese_content", "退款", retrieval.SearchOptions{}, []uint64{chinese.Chunk.ChunkID}},
		{"single_character", "税", retrieval.SearchOptions{}, []uint64{chinese.Chunk.ChunkID}},
		{"mixed_text", "API接口", retrieval.SearchOptions{}, []uint64{chinese.Chunk.ChunkID}},
		{"section_only", "", retrieval.SearchOptions{Section: "投稿审核"}, []uint64{chinese.Chunk.ChunkID}},
		{"exact_article", "", retrieval.SearchOptions{ArticleNumber: "第十条"}, []uint64{chinese.Chunk.ChunkID}},
		{"article_is_not_prefix", "", retrieval.SearchOptions{ArticleNumber: "第十"}, nil},
		{"category", "退款", retrieval.SearchOptions{CategoryID: chinese.Chunk.CategoryID}, []uint64{chinese.Chunk.ChunkID}},
		{"wrong_category", "退款", retrieval.SearchOptions{CategoryID: titleHit.Chunk.CategoryID}, nil},
		{"wrong_document", "退款", retrieval.SearchOptions{DocumentID: titleHit.Chunk.DocumentID}, nil},
		{"combined", "退款", retrieval.SearchOptions{
			DocumentID: chinese.Chunk.DocumentID,
			CategoryID: chinese.Chunk.CategoryID,
			Section:    "投稿审核", ArticleNumber: "第十条",
		}, []uint64{chinese.Chunk.ChunkID}},
		{"combined_mismatch", "退款", retrieval.SearchOptions{
			Section: "不存在章节", ArticleNumber: "第十条",
		}, nil},
		{"no_results", "zzunmatchedsearchtoken", retrieval.SearchOptions{}, nil},
	} {
		runCase(t, tc.name, func(t *testing.T) {
			liveWantIDs(t, retrieve(t, tc.query, tc.opts), tc.want...)
		})
	}

	runCase(t, "controlled_title_weight", func(t *testing.T) {
		got := retrieve(t, "ranktoken", retrieval.SearchOptions{Limit: 2})
		liveWantIDs(t, got, titleHit.Chunk.ChunkID, bodyHit.Chunk.ChunkID)
		if got[0].ChunkID != titleHit.Chunk.ChunkID || got[0].Score <= got[1].Score {
			t.Fatalf("对照语料标题加权排序不符：%+v", got)
		}
		t.Logf("CONTROLLED_TITLE_SCORE=%g BODY_SCORE=%g", got[0].Score, got[1].Score)
	})

	runCase(t, "highlight_html_encoding", func(t *testing.T) {
		got := retrieve(t, "退款", retrieval.SearchOptions{Highlight: true})
		liveWantIDs(t, got, chinese.Chunk.ChunkID)
		if got[0].Content != chinese.Chunk.Content {
			t.Fatal("高亮覆盖了原始正文")
		}
		fragments := got[0].Highlights["content"]
		if len(fragments) == 0 {
			t.Fatal("真实ES未返回正文高亮")
		}
		joined := strings.Join(fragments, "")
		if !strings.Contains(joined, "<em>") ||
			!strings.Contains(joined, "&lt;script&gt;") ||
			strings.Contains(joined, "<script>") {
			t.Fatalf("真实高亮编码不符合契约：%q", joined)
		}

		section := retrieve(t, "", retrieval.SearchOptions{
			Section: "投稿审核", Highlight: true,
		})
		liveWantIDs(t, section, chinese.Chunk.ChunkID)
		if !strings.Contains(strings.Join(section[0].Highlights["section_path"], ""), "<em>") {
			t.Fatal("显式章节条件未参与高亮")
		}

		plain := retrieve(t, "退款", retrieval.SearchOptions{})
		if plain[0].Highlights == nil || len(plain[0].Highlights) != 0 {
			t.Fatal("关闭高亮时应返回空映射")
		}
	})

	runCase(t, "actual_es_time_boundaries", func(t *testing.T) {
		defer func() { queryAt = s.at }()
		queryAt = s.at.Add(-time.Millisecond)
		liveWantIDs(t, retrieve(t, "boundarytoken", retrieval.SearchOptions{}),
			expiring.Chunk.ChunkID)
		queryAt = s.at
		liveWantIDs(t, retrieve(t, "boundarytoken", retrieval.SearchOptions{}),
			effective.Chunk.ChunkID, expiring.Chunk.ChunkID)
		queryAt = s.at.Add(time.Millisecond)
		liveWantIDs(t, retrieve(t, "boundarytoken", retrieval.SearchOptions{}),
			effective.Chunk.ChunkID)
	})

	runCase(t, "actual_es_current_state_filters", func(t *testing.T) {
		before := retrieve(t, "statefiltertoken", retrieval.SearchOptions{})
		liveWantIDs(t, before,
			stateDocs[0].Chunk.ChunkID, stateDocs[1].Chunk.ChunkID, stateDocs[2].Chunk.ChunkID)

		// 只修改独立测试索引，构造违背当前状态的索引记录。
		// MySQL仍有效，因此空候选必须由ES过滤产生。
		e.patch(t, index, stateDocs[0].Chunk.ChunkID, map[string]any{"is_current": false})
		e.patch(t, index, stateDocs[1].Chunk.ChunkID, map[string]any{"status": "draft"})
		e.patch(t, index, stateDocs[2].Chunk.ChunkID,
			map[string]any{"withdrawn_at": s.at.Format(time.RFC3339Nano)})

		got, err := esClient.SearchCandidates(context.Background(), "statefiltertoken",
			retrieval.SearchOptions{Limit: 10}, s.at)
		must(t, err)
		if got == nil || len(got) != 0 {
			t.Fatal("ES当前状态过滤没有排除测试记录")
		}
		t.Log("ES_CURRENT_STATUS_WITHDRAWAL_FILTERS=true")
	})

	runCase(t, "mysql_staleness_and_bounded_candidates", func(t *testing.T) {
		first, err := esClient.SearchCandidates(context.Background(), "staletoken",
			retrieval.SearchOptions{Limit: 1}, s.at)
		must(t, err)
		if len(first) != 5 {
			t.Fatalf("Limit=1应取得5个候选，实际%d", len(first))
		}
		selected := make(map[uint64]bool)
		for i, hit := range first {
			c, ok := staleDocs[hit.Chunk.ChunkID]
			if !ok {
				t.Fatal("收到测试集合之外的候选")
			}
			selected[c.Chunk.ChunkID] = true
			switch i {
			case 0:
				must(t, s.db.Model(&model.KnowledgeDocument{}).
					Where("id = ?", c.Chunk.DocumentID).
					Update("publication_revision", 2).Error)
			case 1:
				must(t, s.db.Model(&model.KnowledgeDocument{}).
					Where("id = ?", c.Chunk.DocumentID).
					Update("current_version_id", nil).Error)
			case 2:
				must(t, s.db.Model(&model.RuleVersion{}).
					Where("id = ?", c.Chunk.VersionID).
					Update("withdrawn_at", s.at).Error)
			case 3:
				must(t, s.db.Model(&model.RuleVersion{}).
					Where("id = ?", c.Chunk.VersionID).
					Update("expires_at", s.at).Error)
			case 4:
				must(t, s.db.Model(&model.RuleVersion{}).
					Where("id = ?", c.Chunk.VersionID).
					Update("category_id", chinese.Chunk.CategoryID).Error)
			}
		}
		var remaining uint64
		for id := range staleDocs {
			if !selected[id] {
				remaining = id
			}
		}
		if remaining == 0 {
			t.Fatal("缺少候选预算外的有效测试片段")
		}

		// ES未重建，仍返回原五个候选；MySQL复核全部排除。
		again, err := esClient.SearchCandidates(context.Background(), "staletoken",
			retrieval.SearchOptions{Limit: 1}, s.at)
		must(t, err)
		if len(again) != len(first) {
			t.Fatal("MySQL更新意外改变了ES候选")
		}
		for i := range first {
			if again[i].Chunk.ChunkID != first[i].Chunk.ChunkID {
				t.Fatal("ES候选顺序发生意外变化")
			}
		}
		liveWantIDs(t, retrieve(t, "staletoken", retrieval.SearchOptions{Limit: 1}))
		liveWantIDs(t, retrieve(t, "staletoken", retrieval.SearchOptions{Limit: 2}), remaining)
		t.Logf("STALE_CANDIDATES=5 LIMIT1_RESULT=0 LIMIT2_RESULT=1 VALID_ID=%d", remaining)
	})

	user := model.User{
		Username: "live_search_user", PasswordHash: "not-a-login-password",
		Role: model.RoleUser,
	}
	must(t, s.db.Create(&user).Error)
	tokens, err := auth.NewTokenManager(strings.Repeat("l", 32), "live-search", time.Hour)
	must(t, err)
	token, err := tokens.Issue(user.ID)
	must(t, err)
	limiter := limiterFunc(func(context.Context, uint64) (cache.RateLimitResult, error) {
		return cache.RateLimitResult{Allowed: true}, nil
	})
	api := func(r retrieval.Retriever) http.Handler {
		return router.NewWithRetriever(
			config.Config{AppName: "live-search", Env: "test"},
			s.db, tokens, nil, limiter, nil, nil, nil, r,
		)
	}

	runCase(t, "actual_handler_es_mysql_chain", func(t *testing.T) {
		body := liveHTTPGet(t, api(search), token,
			"q="+url.QueryEscape("退款")+"&highlight=true", 200, "")
		if body.Count != 1 || body.Limit != 10 ||
			body.Items[0].ChunkID != chinese.Chunk.ChunkID ||
			body.Items[0].Content != chinese.Chunk.Content ||
			len(body.Items[0].Highlights["content"]) == 0 {
			t.Fatalf("完整HTTP链路响应不符：%+v", body)
		}
		empty := liveHTTPGet(t, api(search), token, "q=zzunmatchedsearchtoken", 200, "")
		if empty.Count != 0 {
			t.Fatal("无结果查询不应返回条款")
		}
	})

	runCase(t, "real_missing_alias_http_error", func(t *testing.T) {
		missing := liveClient(t, liveESEndpoint, alias+"_missing")
		target, err := missing.GetAliasTarget(context.Background())
		must(t, err)
		if target != "" {
			t.Fatal("预期隔离的缺失Alias实际存在")
		}
		service, err := retrieval.NewServiceWithClock(
			missing, repo, func() time.Time { return s.at })
		must(t, err)
		liveHTTPGet(t, api(service), token, "q=test", 503, "search_index_not_ready")
	})

	runCase(t, "tcp_failure_is_not_empty_success", func(t *testing.T) {
		// 明确的TCP断连故障注入，不停止共享ES容器。
		broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Error("测试服务器不支持Hijack")
				return
			}
			conn, _, err := hijacker.Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = conn.Close()
		}))
		defer broken.Close()
		brokenClient := liveClient(t, broken.URL, alias)
		service, err := retrieval.NewServiceWithClock(
			brokenClient, repo, func() time.Time { return s.at })
		must(t, err)
		got, err := service.Retrieve(context.Background(), "test", retrieval.SearchOptions{})
		if got != nil || !errors.Is(err, elasticsearch.ErrUnavailable) {
			t.Fatalf("连接失败没有明确返回：got=%v err=%v", got, err)
		}
		brokenAPI := api(service)
		liveHTTPGet(t, brokenAPI, token, "q=test", 503, "search_unavailable")

		recorder := httptest.NewRecorder()
		brokenAPI.ServeHTTP(recorder, httptest.NewRequest("GET", "/health", nil))
		if recorder.Code != 200 {
			t.Fatal("检索依赖失败影响了健康路由")
		}
		t.Log("FAULT_INJECTION=TCP_CLOSE HTTP_STATUS=503 HEALTH_STATUS=200")
	})

	runCase(t, "real_mysql_failure_after_real_es_hit", func(t *testing.T) {
		candidates, err := esClient.SearchCandidates(context.Background(), "退款",
			retrieval.SearchOptions{Limit: 10}, s.at)
		must(t, err)
		if len(candidates) != 1 {
			t.Fatal("故障测试前未取得真实ES候选")
		}
		must(t, s.db.Exec("RENAME TABLE knowledge_chunks TO search_hidden_chunks").Error)
		t.Cleanup(func() {
			must(t, s.db.Exec("RENAME TABLE search_hidden_chunks TO knowledge_chunks").Error)
		})
		liveHTTPGet(t, api(search), token,
			"q="+url.QueryEscape("退款"), 503, "search_verification_failed")
	})

	runCase(t, "final_resource_record", func(t *testing.T) {
		target, err := esClient.GetAliasTarget(context.Background())
		must(t, err)
		if target != index || e.count(t, index) != 14 {
			t.Fatal("最终隔离Alias或索引计数异常")
		}
		t.Logf("PRESERVED_TEST_ALIAS=%s PRESERVED_TEST_INDEX=%s COUNT=14", alias, index)
		t.Log("REAL_ES_AND_MYSQL_SEARCH=true LIMITER_BACKEND=stub")
		t.Log("CHINESE_FIXTURE_ID=" + strconv.FormatUint(chinese.Chunk.ChunkID, 10))
	})
}
