//go:build windows && indexexportintegration && vectorimportintegration && vectorhydrationintegration && vectorsearchintegration

package repository

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"video-support-agent/internal/auth"
	"video-support-agent/internal/config"
	"video-support-agent/internal/embedding"
	"video-support-agent/internal/milvus"
	"video-support-agent/internal/model"
	"video-support-agent/internal/retrieval"
	"video-support-agent/internal/vectorrebuild"
)

type veCase struct {
	Name     string `json:"name"`
	Query    string `json:"query"`
	Fragment string `json:"fragment"`
	Article  string `json:"article"`
	ChunkID  uint64 `json:"expected_chunk_id"`
	Document uint64 `json:"document_id"`
	Category uint64 `json:"category_id"`
}

type veItem struct {
	ChunkID        uint64              `json:"chunk_id"`
	DocumentID     uint64              `json:"document_id"`
	CategoryID     uint64              `json:"category_id"`
	Content        string              `json:"content"`
	ArticleContent string              `json:"article_content"`
	Score          float64             `json:"score"`
	Highlights     map[string][]string `json:"highlights"`
}

type veReply struct {
	Items []veItem `json:"items"`
	Count int      `json:"count"`
	Limit int      `json:"limit"`
}

func veGet(
	t *testing.T, client *http.Client, dir, label, endpoint, token string,
	wantStatus int,
) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	vectorLiveMust(t, err)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := client.Do(req)
	vectorLiveMust(t, err)
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, (2<<20)+1))
	vectorLiveMust(t, err)
	vectorLiveRequire(t, len(body) <= 2<<20, "HTTP response exceeded evidence budget")
	vectorLiveSave(t, dir, label+".json", body)
	vectorLiveSave(t, dir, label+".status",
		[]byte(strconv.Itoa(response.StatusCode)))
	vectorLiveRequire(t, response.StatusCode == wantStatus,
		fmt.Sprintf("%s: status=%d want=%d; inspect saved response",
			label, response.StatusCode, wantStatus))
	return body
}

func veDecode(t *testing.T, body []byte, limit int) veReply {
	t.Helper()
	var reply veReply
	vectorLiveMust(t, json.Unmarshal(body, &reply))
	vectorLiveRequire(t,
		reply.Items != nil && reply.Count == len(reply.Items) &&
			reply.Limit == limit && reply.Count <= limit,
		"invalid success envelope")
	return reply
}

func veCheckItem(t *testing.T, item veItem, expected veCase) {
	t.Helper()
	vectorLiveRequire(t,
		item.ChunkID == expected.ChunkID &&
			item.DocumentID == expected.Document &&
			item.CategoryID == expected.Category &&
			item.Content == expected.Fragment &&
			item.ArticleContent == expected.Article &&
			item.Content != item.ArticleContent &&
			item.Highlights != nil && len(item.Highlights) == 0 &&
			!math.IsNaN(item.Score) && !math.IsInf(item.Score, 0) &&
			item.Score >= -1 && item.Score <= 1,
		"retrieved identity, bodies, score or highlights differ")
}

func TestVectorSearchLive(t *testing.T) {
	vectorLiveRequire(t, os.Getenv("RUN_VECTOR_SEARCH_LIVE") == "1",
		"RUN_VECTOR_SEARCH_LIVE=1 is required")
	root := os.Getenv("VECTOR_SEARCH_PROJECT_ROOT")
	dir := os.Getenv("VECTOR_SEARCH_EVIDENCE")
	redisAddress := os.Getenv("VECTOR_SEARCH_REDIS_ADDR")
	vectorLiveRequire(t, filepath.IsAbs(root) && filepath.IsAbs(dir),
		"absolute project/evidence paths required")
	host, _, err := net.SplitHostPort(redisAddress)
	vectorLiveMust(t, err)
	vectorLiveRequire(t, host == "127.0.0.1",
		"isolated Redis must use loopback")

	// CTRL_BREAK targets only the new child process group.
	// An attached Windows console is required for this actual signal test.
	kernel := syscall.NewLazyDLL("kernel32.dll")
	consoleCP, _, _ := kernel.NewProc("GetConsoleCP").Call()
	vectorLiveRequire(t, consoleCP != 0,
		"run this script from a Windows Terminal PowerShell console")
	sendBreak := kernel.NewProc("GenerateConsoleCtrlEvent")
	vectorLiveMust(t, sendBreak.Find())

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	client := &http.Client{
		Timeout: 45 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	t.Cleanup(client.CloseIdleConnections)
	h := &vectorLiveHTTP{
		t: t, dir: dir, client: client, mutable: make(map[string]bool),
	}
	snapshot := func() map[string]any {
		result := h.protectedSnapshot()
		result["s02_alias"] = h.call("aliases/describe", map[string]any{
			"aliasName": "vec_import_it_999bd24a8ab257d7",
		})
		result["s02_count"] = h.count(
			"rule_chunks_vec_v1_005160370c3e73bc51a75dcd80f4f009")
		return result
	}
	before := snapshot()
	vectorLiveSave(t, dir, "protected-before.json", vectorLiveJSON(t, before))
	protectedChecked := false
	checkProtected := func() {
		after := snapshot()
		vectorLiveSave(t, dir, "protected-after.json", vectorLiveJSON(t, after))
		vectorLiveRequire(t, reflect.DeepEqual(before, after),
			"protected resources changed")
		protectedChecked = true
	}
	t.Cleanup(func() {
		if !protectedChecked {
			checkProtected()
		}
	})

	// Reuse the isolated database creation helper, which retains its database.
	s := vhSuite(t)
	s.db = s.db.WithContext(ctx)
	s.writer = s.writer.WithContext(ctx)
	s.at = time.Now().UTC().Truncate(time.Millisecond)
	vectorLiveMust(t, s.db.AutoMigrate(&model.User{}))

	cases := []veCase{
		{
			Name:     "password",
			Query:    "忘记登录密码了，怎样重新设置？",
			Fragment: "【虚构测试规则】忘记账号登录密码时，使用绑定手机接收验证码，验证身份后重设密码。",
			Article:  "【虚构测试规则，不作为官方依据】账号密码找回：用户忘记登录密码时，进入找回密码页面，使用绑定手机接收验证码，验证身份后设置新密码。验证码不得转交他人。",
		},
		{
			Name:     "appeal",
			Query:    "投稿被判违规，我觉得判错了怎么办？",
			Fragment: "【虚构测试规则】视频投稿审核未通过且认为判定有误时，可以在审核通知中提交申诉并附上说明材料。",
			Article:  "【虚构测试规则，不作为官方依据】视频审核申诉：视频投稿审核未通过且认为判定有误时，用户可以在审核通知中提交申诉，写明异议并附上说明材料。申诉结果通过站内通知告知。",
		},
		{
			Name:     "withdrawal",
			Query:    "创作收入怎么转到自己的银行卡？",
			Fragment: "【虚构测试规则】创作收益提现前须完成实名认证并绑定本人银行卡，然后在收益中心申请提现。",
			Article:  "【虚构测试规则，不作为官方依据】创作收益提现：用户须完成实名认证并绑定本人银行卡，然后在收益中心填写提现金额并确认申请。处理状态可在提现记录中查看。",
		},
	}
	fixtures := make([]exportMySQLFixture, len(cases))
	for i := range cases {
		f := s.fixture(t)
		// Synthetic fixture construction before its first vector export.
		vhUpdate(t, s, "rule_versions", f.version.ID, "title", cases[i].Name)
		vhUpdate(t, s, "knowledge_chunks", f.chunk.ID, "content", cases[i].Fragment)
		vhUpdate(t, s, "rule_articles", f.article.ID, "content", cases[i].Article)
		fixtures[i] = f
		cases[i].ChunkID = f.chunk.ID
		cases[i].Document = f.document.ID
		cases[i].Category = f.category.ID
	}
	// Expectations are saved before any semantic search.
	vectorLiveSave(t, dir, "semantic-expectations.json", vectorLiveJSON(t, cases))

	var random [16]byte
	_, err = rand.Read(random[:])
	vectorLiveMust(t, err)
	suffix := hex.EncodeToString(random[:])
	alias := "vec_search_it_" + suffix[:16]
	collection := "rule_chunks_vec_v1_" + suffix

	embeddingConfig := config.EmbeddingConfig{
		URL: "http://127.0.0.1:11434", Model: "bge-m3:567m",
		ModelDigest: vectorLiveDigest, Dimensions: "1024",
		ContextLength: "8192", Timeout: "120s",
	}
	embedder, err := embedding.NewClient(embeddingConfig)
	vectorLiveMust(t, err)
	t.Cleanup(embedder.Close)

	vectorClient, err := milvus.NewClient(config.MilvusConfig{
		URL: "http://127.0.0.1:19530", Database: "default",
		CollectionAlias: alias, Timeout: "60s",
	})
	vectorLiveMust(t, err)
	t.Cleanup(vectorClient.Close)
	old, err := vectorClient.GetAliasTarget(ctx)
	vectorLiveMust(t, err)
	vectorLiveRequire(t, old == "", "random test alias already exists")

	repository := NewKnowledgeRepository(s.db)
	sourceBefore, err := repository.ExportCurrent(ctx)
	vectorLiveMust(t, err)
	vectorLiveSave(t, dir, "source-before.json", vectorLiveJSON(t, sourceBefore))

	result, err := vectorrebuild.Rebuild(ctx, repository, embedder, vectorClient,
		vectorrebuild.Options{
			NewCollection: collection, ExpectedDatabase: s.name,
			ExpectedServerUUID: s.uuid, ExpectedDigest: vectorLiveDigest,
		},
		func(stage vectorrebuild.Result) error {
			data, err := json.MarshalIndent(stage, "", "  ")
			if err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(dir, "rebuild-"+stage.Stage+".json"), data, 0600)
		})
	vectorLiveMust(t, err)
	vectorLiveRequire(t,
		result.Count == 3 && result.Stage == "complete" &&
			result.Publication.State == milvus.PublicationApplied &&
			result.Identity.ServerVersion == "0.40.13",
		"real rebuild/model identity did not meet baseline")

	vectorLiveSave(t, dir, "resources.json", vectorLiveJSON(t, map[string]any{
		"database": s.name, "alias": alias, "collection": collection,
		"server_uuid": s.uuid, "redis_address": redisAddress,
		"fixture_kind": "synthetic_not_official", "resources_retained": true,
	}))
	t.Logf("TEST_DATABASE=%s TEST_ALIAS=%s TEST_COLLECTION=%s",
		s.name, alias, collection)

	searcher, err := milvus.NewSearcher(embedder, vectorClient, 35*time.Second)
	vectorLiveMust(t, err)
	retriever, err := retrieval.NewMilvusRetriever(searcher, repository, 35*time.Second)
	vectorLiveMust(t, err)

	for _, tc := range cases {
		got, err := retriever.Retrieve(ctx, tc.Query, retrieval.SearchOptions{Limit: 1})
		vectorLiveMust(t, err)
		vectorLiveSave(t, dir, "direct-"+tc.Name+".json", vectorLiveJSON(t, got))
		vectorLiveRequire(t, len(got) == 1 && got[0].Chunk.ChunkID == tc.ChunkID &&
			got[0].Chunk.Content == tc.Fragment &&
			got[0].ArticleContent == tc.Article,
			"predeclared semantic top-1 expectation failed: "+tc.Name)
	}

	prepared, err := milvus.PrepareQuery(ctx, cases[0].Query,
		retrieval.SearchOptions{Limit: 1}, time.Now(), embedder)
	vectorLiveMust(t, err)
	prepared.Identity.Digest = strings.Repeat("0", 64)
	_, err = vectorClient.SearchPrepared(ctx, prepared)
	vectorLiveRequire(t, errors.Is(err, milvus.ErrResponse),
		"real collection accepted mismatched model identity")
	vectorLiveSave(t, dir, "model-contract-check.json",
		[]byte(`{"mismatched_model_rejected":true}`))

	sourceAfter, err := repository.ExportCurrent(ctx)
	vectorLiveMust(t, err)
	vectorLiveRequire(t,
		reflect.DeepEqual(sourceBefore.Documents, sourceAfter.Documents),
		"import/search changed source documents")

	user := model.User{
		Username: "vector_live_user", Role: model.RoleUser,
		PasswordHash: "isolated-test-placeholder",
	}
	vectorLiveMust(t, s.db.Create(&user).Error)
	var secretBytes [32]byte
	_, err = rand.Read(secretBytes[:])
	vectorLiveMust(t, err)
	jwtSecret := hex.EncodeToString(secretBytes[:])
	tokens, err := auth.NewTokenManager(jwtSecret, "vector-live", time.Hour)
	vectorLiveMust(t, err)
	token, err := tokens.Issue(user.ID)
	vectorLiveMust(t, err)

	// Successful requests are transparently forwarded to real dependencies.
	var embeddingCalls, milvusCalls, unexpectedCalls atomic.Int32
	embeddingTarget, _ := url.Parse("http://127.0.0.1:11434")
	embeddingProxy := httputil.NewSingleHostReverseProxy(embeddingTarget)
	embeddingServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		embeddingCalls.Add(1)
		embeddingProxy.ServeHTTP(w, r)
	}))
	t.Cleanup(embeddingServer.Close)

	milvusTarget, _ := url.Parse("http://127.0.0.1:19530")
	milvusProxy := httputil.NewSingleHostReverseProxy(milvusTarget)
	var fault atomic.Int32
	var forbiddenWrites atomic.Int32
	allowedPaths := map[string]bool{
		"/v2/vectordb/aliases/list":               true,
		"/v2/vectordb/aliases/describe":           true,
		"/v2/vectordb/collections/describe":       true,
		"/v2/vectordb/indexes/describe":           true,
		"/v2/vectordb/collections/get_load_state": true,
		"/v2/vectordb/entities/search":            true,
	}
	milvusServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		milvusCalls.Add(1)
		if r.Method != http.MethodPost || !allowedPaths[r.URL.Path] {
			forbiddenWrites.Add(1)
			http.Error(w, "forbidden test operation", 403)
			return
		}
		switch fault.Load() {
		case 1:
			http.Error(w, "private-backend-detail", 503)
		case 2:
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		case 3:
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"broken":`)
		default:
			milvusProxy.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(milvusServer.Close)

	// No ES fallback or chat-model request is allowed in vector retrieval.
	unexpectedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		unexpectedCalls.Add(1)
		http.Error(w, "unexpected dependency call", 503)
	}))
	t.Cleanup(unexpectedServer.Close)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	vectorLiveMust(t, err)
	port := listener.Addr().(*net.TCPAddr).Port
	vectorLiveMust(t, listener.Close())
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	appName := "vector-live-" + suffix[:16]
	binary := filepath.Join(dir, "vector-api.exe")
	info, err := os.Stat(binary)
	vectorLiveMust(t, err)
	vectorLiveRequire(t, info.Mode().IsRegular(), "API binary missing")

	connectionCount := func() int {
		var count int
		vectorLiveMust(t, s.pool.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM information_schema.PROCESSLIST WHERE DB = ?",
			s.name).Scan(&count))
		return count
	}
	baselineConnections := connectionCount()

	stdoutPath := filepath.Join(dir, "api.stdout.log")
	stderrPath := filepath.Join(dir, "api.stderr.log")
	stdout, err := os.Create(stdoutPath)
	vectorLiveMust(t, err)
	t.Cleanup(func() { _ = stdout.Close() })
	stderr, err := os.Create(stderrPath)
	vectorLiveMust(t, err)
	t.Cleanup(func() { _ = stderr.Close() })

	command := exec.Command(binary)
	command.Dir = root
	command.Stdout, command.Stderr = stdout, stderr
	command.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: 0x00000200, // CREATE_NEW_PROCESS_GROUP
		HideWindow:    true,
	}
	command.Env = vectorLiveEnvironment(map[string]string{
		"APP_NAME": appName, "APP_ENV": "production",
		"APP_PORT": strconv.Itoa(port), "LOG_LEVEL": "info",
		"TRUSTED_PROXIES": "",
		"DB_HOST":         "127.0.0.1", "DB_PORT": "13308",
		"DB_USER": "root", "DB_PASSWORD": os.Getenv("ES_TEST_MYSQL_ROOT_PASSWORD"),
		"DB_NAME":    s.name,
		"REDIS_ADDR": redisAddress, "REDIS_PASSWORD": "",
		"JWT_SECRET": jwtSecret, "JWT_ISSUER": "vector-live", "JWT_TTL": "1h",
		"LLM_BASE_URL": unexpectedServer.URL, "LLM_MODEL": "unused-test-model",
		"LLM_API_KEY": "unused-isolated-key", "LLM_TIMEOUT": "5s",
		"ES_URL": unexpectedServer.URL, "ES_INDEX_ALIAS": "unused_test_alias",
		"ES_TIMEOUT":             "2s",
		"EMBEDDING_URL":          embeddingServer.URL,
		"EMBEDDING_MODEL":        "bge-m3:567m",
		"EMBEDDING_MODEL_DIGEST": vectorLiveDigest,
		"EMBEDDING_DIMENSIONS":   "1024", "EMBEDDING_CONTEXT_LENGTH": "8192",
		"EMBEDDING_TIMEOUT": "60s",
		"MILVUS_URL":        milvusServer.URL, "MILVUS_DATABASE": "default",
		"MILVUS_COLLECTION_ALIAS": alias, "MILVUS_TIMEOUT": "2s",
	})
	vectorLiveMust(t, command.Start())
	done := make(chan struct{})
	var processErr error
	go func() {
		processErr = command.Wait()
		close(done)
	}()
	t.Cleanup(func() {
		select {
		case <-done:
		default:
			// Failure cleanup only. This never counts as graceful shutdown.
			_ = command.Process.Kill()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Error("test API process did not exit after failure cleanup")
			}
		}
	})

	ready := false
	healthClient := &http.Client{Timeout: time.Second}
	t.Cleanup(healthClient.CloseIdleConnections)
	readyDeadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(readyDeadline) {
		select {
		case <-done:
			t.Fatalf("API exited during startup: %v; inspect api logs", processErr)
		default:
		}
		response, getErr := healthClient.Get(baseURL + "/health")
		if getErr == nil {
			var health struct {
				Service string `json:"service"`
				Status  string `json:"status"`
			}
			decodeErr := json.NewDecoder(response.Body).Decode(&health)
			_ = response.Body.Close()
			if decodeErr == nil && response.StatusCode == 200 &&
				health.Service == appName && health.Status == "ok" {
				ready = true
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	vectorLiveRequire(t, ready, "API health did not become ready")
	vectorLiveRequire(t, embeddingCalls.Load() == 0 &&
		milvusCalls.Load() == 0 && unexpectedCalls.Load() == 0,
		"startup unexpectedly probed retrieval/chat dependencies")

	endpoint := func(query string, values url.Values) string {
		if values == nil {
			values = make(url.Values)
		}
		values.Set("q", query)
		return baseURL + "/api/v1/knowledge/vector-search?" + values.Encode()
	}

	veGet(t, client, dir, "http-unauthorized",
		endpoint(cases[0].Query, nil), "", 401)
	vectorLiveRequire(t, embeddingCalls.Load() == 0 && milvusCalls.Load() == 0,
		"unauthenticated request reached vector dependencies")

	for _, tc := range cases {
		body := veGet(t, client, dir, "http-"+tc.Name,
			endpoint(tc.Query, url.Values{"limit": {"1"}}), token, 200)
		reply := veDecode(t, body, 1)
		vectorLiveRequire(t, reply.Count == 1, "semantic query returned no result")
		veCheckItem(t, reply.Items[0], tc)
	}

	body := veGet(t, client, dir, "http-ranked",
		endpoint(cases[0].Query, url.Values{"limit": {"3"}}), token, 200)
	ranked := veDecode(t, body, 3)
	vectorLiveRequire(t, ranked.Count == 3, "expected all three bounded candidates")
	seen := make(map[uint64]bool)
	for i, item := range ranked.Items {
		found := false
		for _, tc := range cases {
			if item.ChunkID == tc.ChunkID {
				veCheckItem(t, item, tc)
				found = true
			}
		}
		vectorLiveRequire(t, found && !seen[item.ChunkID], "unknown or duplicate result")
		seen[item.ChunkID] = true
		if i > 0 {
			vectorLiveRequire(t, ranked.Items[i-1].Score+1e-6 >= item.Score,
				"similarity order was not preserved")
		}
	}

	for _, field := range []string{"document_id", "category_id"} {
		id := cases[1].Document
		if field == "category_id" {
			id = cases[1].Category
		}
		body := veGet(t, client, dir, "http-filter-"+field,
			endpoint(cases[0].Query, url.Values{
				field: {strconv.FormatUint(id, 10)}, "limit": {"3"},
			}), token, 200)
		reply := veDecode(t, body, 3)
		vectorLiveRequire(t, reply.Count == 1, "filter did not restrict results")
		veCheckItem(t, reply.Items[0], cases[1])
	}

	body = veGet(t, client, dir, "http-empty",
		endpoint(cases[0].Query, url.Values{
			"document_id": {"18446744073709551615"}, "limit": {"1"},
		}), token, 200)
	vectorLiveRequire(t, veDecode(t, body, 1).Count == 0,
		"nonexistent document filter did not produce an empty result")

	for _, failure := range []struct {
		mode   int32
		name   string
		status int
		code   string
	}{
		{1, "unavailable", 503, "vector_search_unavailable"},
		{2, "timeout", 504, "request_timeout"},
		{3, "bad_response", 502, "invalid_vector_backend_response"},
	} {
		fault.Store(failure.mode)
		body := veGet(t, client, dir, "http-fault-"+failure.name,
			endpoint(cases[0].Query, nil), token, failure.status)
		var response map[string]any
		vectorLiveMust(t, json.Unmarshal(body, &response))
		vectorLiveRequire(t, len(response) == 2 && response["code"] == failure.code &&
			!strings.Contains(string(body), "private-backend-detail"),
			"dependency failure was disguised or leaked details")
		fault.Store(0)
	}
	body = veGet(t, client, dir, "http-recovered",
		endpoint(cases[0].Query, url.Values{"limit": {"1"}}), token, 200)
	recovered := veDecode(t, body, 1)
	vectorLiveRequire(t, recovered.Count == 1, "request did not recover after injected fault")
	veCheckItem(t, recovered.Items[0], cases[0])

	// Change only the newly created fixture's publication state.
	for i, mode := range []string{"withdrawn", "revision_changed"} {
		if mode == "withdrawn" {
			vhUpdate(t, s, "knowledge_documents",
				fixtures[i].document.ID, "current_version_id", nil)
		} else {
			vhUpdate(t, s, "knowledge_documents",
				fixtures[i].document.ID, "publication_revision", 2)
		}
		opts := retrieval.SearchOptions{DocumentID: cases[i].Document, Limit: 1}
		candidates, err := searcher.SearchCandidates(ctx, cases[i].Query, opts, time.Now())
		vectorLiveMust(t, err)
		vectorLiveRequire(t, len(candidates) == 1 &&
			candidates[0].Chunk.ChunkID == cases[i].ChunkID,
			"stale-filter fixture no longer exists in Milvus")
		vectorLiveSave(t, dir, "stale-candidates-"+mode+".json",
			vectorLiveJSON(t, candidates))
		body := veGet(t, client, dir, "http-stale-"+mode,
			endpoint(cases[i].Query, url.Values{
				"document_id": {strconv.FormatUint(cases[i].Document, 10)},
				"limit":       {"1"},
			}), token, 200)
		vectorLiveRequire(t, veDecode(t, body, 1).Count == 0,
			"MySQL rejected state was returned through HTTP")
	}
	vectorLiveRequire(t, h.count(collection) == 3,
		"read-side stale filtering changed Milvus data")
	vectorLiveRequire(t, forbiddenWrites.Load() == 0 &&
		unexpectedCalls.Load() == 0 &&
		embeddingCalls.Load() > 0 && milvusCalls.Load() > 0,
		"unexpected write/fallback/chat call or missing real dependency calls")

	// Windows maps CTRL_BREAK_EVENT to os.Interrupt in Go.
	ok, _, signalErr := sendBreak.Call(1, uintptr(command.Process.Pid))
	vectorLiveRequire(t, ok != 0, fmt.Sprintf("send CTRL_BREAK failed: %v", signalErr))
	select {
	case <-done:
	case <-time.After(70 * time.Second):
		t.Fatal("API did not exit through graceful shutdown")
	}
	vectorLiveMust(t, processErr)
	vectorLiveMust(t, stdout.Close())
	vectorLiveMust(t, stderr.Close())
	out, err := os.ReadFile(stdoutPath)
	vectorLiveMust(t, err)
	errOut, err := os.ReadFile(stderrPath)
	vectorLiveMust(t, err)
	logs := string(out) + string(errOut)
	for _, marker := range []string{
		"vector retrieval dependencies initialized",
		"starting API server",
		"shutting down API server",
		"API server stopped",
	} {
		vectorLiveRequire(t, strings.Contains(logs, marker),
			"missing actual API lifecycle log: "+marker)
	}
	for _, secret := range []string{
		jwtSecret, token, os.Getenv("ES_TEST_MYSQL_ROOT_PASSWORD"),
		"unused-isolated-key", "private-backend-detail",
		cases[0].Query, cases[1].Query, cases[2].Query,
	} {
		if secret != "" {
			vectorLiveRequire(t, !strings.Contains(logs, secret),
				"API logs contain a private value")
		}
	}
	vectorLiveRequire(t, connectionCount() <= baselineConnections,
		"API retained MySQL connections after exit")
	connection, dialErr := net.DialTimeout("tcp",
		net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), time.Second)
	if dialErr == nil {
		_ = connection.Close()
		t.Fatal("API port remains open after process exit")
	}

	checkProtected()
	vectorLiveSave(t, dir, "live-result.json", vectorLiveJSON(t, map[string]any{
		"status":   "VECTOR_SEARCH_LIVE_PASSED",
		"database": s.name, "alias": alias, "collection": collection,
		"semantic_cases": 3, "api_exit_code": 0,
		"graceful_shutdown": true, "protected_resources_unchanged": true,
		"resources_retained":          true,
		"embedding_http_calls":        embeddingCalls.Load(),
		"milvus_http_calls":           milvusCalls.Load(),
		"unexpected_dependency_calls": unexpectedCalls.Load(),
	}))
	t.Log("VECTOR_SEARCH_LIVE_PASSED")
}
