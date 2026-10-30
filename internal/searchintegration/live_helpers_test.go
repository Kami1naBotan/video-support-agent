//go:build searchintegration && searchliveintegration

package searchintegration

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"video-support-agent/internal/config"
	"video-support-agent/internal/elasticsearch"
	"video-support-agent/internal/model"
	"video-support-agent/internal/retrieval"
)

const liveESEndpoint = "http://127.0.0.1:9200"
const liveProtectedAlias = "rule_chunks_current"

type liveEnvironment struct {
	httpClient *http.Client
	definition []byte
}

func liveDecode(t *testing.T, data []byte) any {
	t.Helper()
	var value any
	must(t, json.Unmarshal(data, &value))
	return value
}

func (e *liveEnvironment) request(
	t *testing.T, method, path string, body any, expectedStatus int,
) []byte {
	t.Helper()
	var data []byte
	if body != nil {
		var err error
		data, err = json.Marshal(body)
		must(t, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(
		ctx, method, liveESEndpoint+path, bytes.NewReader(data),
	)
	must(t, err)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := e.httpClient.Do(req)
	must(t, err)
	defer resp.Body.Close()
	result, err := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
	must(t, err)
	if len(result) > 4<<20 {
		t.Fatal("ES诊断响应超过上限")
	}
	if resp.StatusCode != expectedStatus {
		t.Fatalf("%s %s: status=%d want=%d",
			method, path, resp.StatusCode, expectedStatus)
	}
	return result
}

func liveClient(t *testing.T, endpoint, alias string) *elasticsearch.Client {
	t.Helper()
	client, err := elasticsearch.NewClient(config.ElasticsearchConfig{
		URL: endpoint, IndexAlias: alias, Timeout: "5s",
	})
	must(t, err)
	t.Cleanup(client.Close)
	return client
}

func newLiveEnvironment(t *testing.T) *liveEnvironment {
	t.Helper()
	if os.Getenv("RUN_SEARCH_LIVE") != "1" {
		t.Fatal("必须显式设置 RUN_SEARCH_LIVE=1；不跳过测试")
	}
	expectedUUID := os.Getenv("SEARCH_EXPECT_ES_CLUSTER_UUID")
	if strings.TrimSpace(expectedUUID) == "" {
		t.Fatal("缺少独立读取的ES集群UUID")
	}
	root := os.Getenv("SEARCH_PROJECT_ROOT")
	if !filepath.IsAbs(root) {
		t.Fatal("SEARCH_PROJECT_ROOT必须为绝对路径")
	}
	definition, err := os.ReadFile(filepath.Join(
		root, "deploy", "elasticsearch", "rule-chunks-v1.json",
	))
	must(t, err)
	client := &http.Client{
		Timeout: 15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	t.Cleanup(client.CloseIdleConnections)
	e := &liveEnvironment{httpClient: client, definition: definition}

	var identity struct {
		ClusterUUID string `json:"cluster_uuid"`
		Version     struct {
			Number string `json:"number"`
		} `json:"version"`
	}
	must(t, json.Unmarshal(e.request(t, "GET", "/", nil, 200), &identity))
	if identity.ClusterUUID != expectedUUID || identity.Version.Number != "8.19.0" {
		t.Fatal("ES实例身份或版本与准备记录不一致")
	}

	var health struct {
		Status   string `json:"status"`
		TimedOut bool   `json:"timed_out"`
	}
	must(t, json.Unmarshal(
		e.request(t, "GET", "/_cluster/health", nil, 200), &health))
	if health.TimedOut || (health.Status != "green" && health.Status != "yellow") {
		t.Fatalf("ES尚未就绪：%+v", health)
	}
	t.Logf("ES_CLUSTER_UUID=%s ES_VERSION=%s", identity.ClusterUUID, identity.Version.Number)

	protected := liveClient(t, liveESEndpoint, liveProtectedAlias)
	target, err := protected.GetAliasTarget(context.Background())
	must(t, err)
	if target == "" {
		t.Fatal("正式Alias不存在，停止测试")
	}
	beforeAlias := liveDecode(t,
		e.request(t, "GET", "/_alias/"+liveProtectedAlias, nil, 200))
	beforeCount := e.count(t, target)

	// 无论测试成功与否，都只读核对正式资源。
	// 若核对失败，父测试会失败，MySQL夹具将保留自建库。
	t.Cleanup(func() {
		afterAlias := liveDecode(t,
			e.request(t, "GET", "/_alias/"+liveProtectedAlias, nil, 200))
		afterCount := e.count(t, target)
		if !reflect.DeepEqual(beforeAlias, afterAlias) || beforeCount != afterCount {
			t.Errorf("正式Alias或计数发生变化：before=%d after=%d",
				beforeCount, afterCount)
			return
		}
		t.Logf("PROTECTED_ALIAS_UNCHANGED=true TARGET=%s COUNT=%d",
			target, beforeCount)
	})
	return e
}

func (e *liveEnvironment) count(t *testing.T, index string) int64 {
	t.Helper()
	var result struct {
		Count  int64 `json:"count"`
		Shards struct {
			Total      int `json:"total"`
			Successful int `json:"successful"`
			Failed     int `json:"failed"`
		} `json:"_shards"`
	}
	must(t, json.Unmarshal(
		e.request(t, "GET", "/"+index+"/_count", nil, 200), &result))
	if result.Shards.Total <= 0 ||
		result.Shards.Successful != result.Shards.Total ||
		result.Shards.Failed != 0 {
		t.Fatal("计数请求没有完整分片结果")
	}
	return result.Count
}

func (e *liveEnvironment) patch(
	t *testing.T, index string, id uint64, fields map[string]any,
) {
	t.Helper()
	if !strings.HasPrefix(index, "rule_chunks_v1_") {
		t.Fatal("只允许修改本测试明确传入的物理索引")
	}
	e.request(t, "POST",
		"/"+index+"/_update/"+strconv.FormatUint(id, 10)+"?refresh=true",
		map[string]any{"doc": fields}, 200)
}

func liveFixture(
	t *testing.T, s *searchSuite, title, section, number, content string,
) retrieval.Candidate {
	t.Helper()
	c := s.fixture(t)
	must(t, s.db.Model(&model.RuleVersion{}).
		Where("id = ?", c.Chunk.VersionID).
		Updates(map[string]any{"title": title, "content": content}).Error)
	must(t, s.db.Model(&model.RuleArticle{}).
		Where("id = ?", c.Chunk.ArticleID).
		Updates(map[string]any{
			"section_path": section, "article_number": number, "content": content,
		}).Error)
	must(t, s.db.Model(&model.KnowledgeChunk{}).
		Where("id = ?", c.Chunk.ChunkID).Update("content", content).Error)
	c.Chunk.Title = title
	c.Chunk.SectionPath = section
	c.Chunk.ArticleNumber = number
	c.Chunk.Content = content
	return c
}

func liveWantIDs(t *testing.T, got []retrieval.RuleChunk, want ...uint64) {
	t.Helper()
	if got == nil {
		t.Fatal("成功结果必须为非nil切片")
	}
	actual := make([]uint64, len(got))
	expected := make([]uint64, len(want))
	copy(expected, want)
	for i, item := range got {
		actual[i] = item.ChunkID
	}
	sort.Slice(actual, func(i, j int) bool { return actual[i] < actual[j] })
	sort.Slice(expected, func(i, j int) bool { return expected[i] < expected[j] })
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("片段集合不符：got=%v want=%v", actual, expected)
	}
}

type liveHTTPResult struct {
	Code  string `json:"code"`
	Count int    `json:"count"`
	Limit int    `json:"limit"`
	Items []struct {
		ChunkID    uint64              `json:"chunk_id"`
		Content    string              `json:"content"`
		Highlights map[string][]string `json:"highlights"`
	} `json:"items"`
}

func liveHTTPGet(
	t *testing.T, handler http.Handler, token, rawQuery string,
	status int, code string,
) liveHTTPResult {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/knowledge/search?"+rawQuery, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if recorder.Code != status {
		t.Fatalf("HTTP status=%d want=%d body=%s",
			recorder.Code, status, recorder.Body.String())
	}
	var result liveHTTPResult
	must(t, json.Unmarshal(recorder.Body.Bytes(), &result))
	if result.Code != code {
		t.Fatalf("错误码不符：%s", recorder.Body.String())
	}
	if status == 200 {
		if result.Items == nil || result.Count != len(result.Items) {
			t.Fatal("成功响应的items/count不一致")
		}
	} else if result.Items != nil {
		t.Fatal("失败响应包含部分候选")
	}
	return result
}
