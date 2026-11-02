//go:build ragstageintegration

package repository_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"video-support-agent/internal/auth"
	"video-support-agent/internal/cache"
	"video-support-agent/internal/config"
	"video-support-agent/internal/database"
	"video-support-agent/internal/llm"
	"video-support-agent/internal/model"
	"video-support-agent/internal/rag"
	"video-support-agent/internal/repository"
	"video-support-agent/internal/retrieval"
	"video-support-agent/internal/router"
	"video-support-agent/internal/service"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const stageQuestion = "虚构测试中，申请复核需要提交什么材料？"
const stageArticle = "【虚构测试，不作为官方依据】申请复核时，需要提交订单编号和问题说明。"
const stageUsername = "rag-stage-user"
const stagePassword = "Rag-stage-only-2026!"

// 只控制候选入口；融合与数据库回表仍使用真实实现。
type stageCandidates struct {
	item retrieval.Candidate
}

func (s stageCandidates) SearchCandidates(
	ctx context.Context,
	_ string,
	_ retrieval.SearchOptions,
	_ time.Time,
) ([]retrieval.Candidate, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return []retrieval.Candidate{s.item}, nil
}

// 本块不重新验收 Redis 限流；JWT、登录和业务路由保持真实。
type stageLimiter struct{}

func (stageLimiter) Allow(context.Context, uint64) (cache.RateLimitResult, error) {
	return cache.RateLimitResult{Allowed: true}, nil
}

func (stageLimiter) AllowIP(context.Context, string) (cache.RateLimitResult, error) {
	return cache.RateLimitResult{Allowed: true}, nil
}

type stageGenerator struct {
	inner llm.Generator
	calls atomic.Int64
}

func (g *stageGenerator) Generate(
	ctx context.Context,
	messages []*schema.Message,
	options ...einomodel.Option,
) (*schema.Message, error) {
	number := g.calls.Add(1)
	prefix := fmt.Sprintf("model-%03d", number)

	input := make([]map[string]string, 0, len(messages))
	for _, message := range messages {
		input = append(input, map[string]string{
			"role": string(message.Role), "content": message.Content,
		})
	}
	if err := stageWrite(prefix+"-input.json", input); err != nil {
		return nil, err
	}

	result, err := g.inner.Generate(ctx, messages, options...)
	record := map[string]any{"has_error": err != nil}
	if result != nil {
		record["content"] = result.Content
		record["role"] = result.Role
		if result.ResponseMeta != nil {
			record["finish_reason"] = result.ResponseMeta.FinishReason
		}
	}
	// 不保存 API key、认证头或底层提供商错误文本。
	if saveErr := stageWrite(prefix+"-output.json", record); saveErr != nil {
		return nil, saveErr
	}
	return result, err
}

type stageApp struct {
	db        *gorm.DB
	server    *httptest.Server
	client    *http.Client
	token     string
	user      *model.User
	candidate retrieval.Candidate
	generator *stageGenerator
}

func stageSeedRule(t *testing.T, db *gorm.DB) retrieval.Candidate {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Millisecond)
	effective := now.Add(-time.Hour)

	document := model.KnowledgeDocument{
		DocumentKey:         "synthetic-rag-stage-rule",
		PublicationRevision: 1,
	}
	category := model.RuleCategory{
		Code: "synthetic-rag-stage", Name: "【虚构测试】分类",
	}
	stageMust(t, db.Create(&document).Error)
	stageMust(t, db.Create(&category).Error)

	version := model.RuleVersion{
		DocumentID:  document.ID,
		CategoryID:  category.ID,
		Version:     "synthetic-v1",
		Title:       "【虚构测试】复核材料规则",
		Content:     stageArticle,
		SourceURL:   "https://www.bilibili.com/__rag_stage_fixture__",
		Status:      model.RuleVersionStatusPublished,
		EffectiveAt: &effective,
		PublishedAt: &now,
	}
	stageMust(t, db.Omit(clause.Associations).Create(&version).Error)

	article := model.RuleArticle{
		VersionID:     version.ID,
		ArticleOrder:  1,
		SectionPath:   "虚构测试章节",
		ArticleNumber: "TEST-1",
		Content:       stageArticle,
	}
	stageMust(t, db.Omit(clause.Associations).Create(&article).Error)

	chunk := model.KnowledgeChunk{
		ArticleID: article.ID,
		Content:   "【虚构测试】复核材料：订单编号和问题说明。",
	}
	stageMust(t, db.Omit(clause.Associations).Create(&chunk).Error)
	stageMust(t, db.Model(&model.KnowledgeDocument{}).
		Where("id=?", document.ID).
		Update("current_version_id", version.ID).Error)

	return retrieval.Candidate{
		PublicationRevision: 1,
		Chunk: retrieval.RuleChunk{
			ChunkID:       chunk.ID,
			DocumentID:    document.ID,
			VersionID:     version.ID,
			ArticleID:     article.ID,
			CategoryID:    category.ID,
			ArticleOrder:  1,
			Version:       version.Version,
			Title:         version.Title,
			SectionPath:   article.SectionPath,
			ArticleNumber: article.ArticleNumber,
			Content:       chunk.Content,
			SourceURL:     version.SourceURL,
			EffectiveAt:   effective,
			Score:         1,
		},
	}
}

func stageLiveApp(t *testing.T, browser bool) *stageApp {
	t.Helper()
	db := stageDB(t)
	stageMust(t, database.Migrate(db))

	cfg := config.Load().LLM
	timeout, err := time.ParseDuration(cfg.Timeout)
	stageMust(t, err)
	stageRequire(t, timeout > 0 && timeout <= 30*time.Second,
		"model timeout must remain within 30 seconds")

	realModel, err := llm.NewChatModel(context.Background(), cfg)
	stageMust(t, err)
	generator := &stageGenerator{inner: realModel}

	candidate := stageSeedRule(t, db)
	source := stageCandidates{item: candidate}
	hybrid, err := retrieval.NewHybridService(
		source, source, repository.NewKnowledgeRepository(db), nil,
	)
	stageMust(t, err)
	responder, err := rag.NewResponder(hybrid, generator, timeout, zap.NewNop())
	stageMust(t, err)

	users := repository.NewUserRepository(db)
	user, err := service.NewAuthService(users).Register(
		context.Background(), stageUsername, stagePassword,
	)
	stageMust(t, err)

	tokens, err := auth.NewTokenManager(
		stageRandom(t), "rag-stage-isolated", 2*time.Hour,
	)
	stageMust(t, err)
	token, err := tokens.Issue(user.ID)
	stageMust(t, err)

	gin.SetMode(gin.TestMode)
	engine := router.New(
		config.Config{AppName: "RAG isolated acceptance"},
		db, tokens, nil, stageLimiter{}, stageLimiter{},
		zap.NewNop(), responder,
	)

	if browser {
		root := os.Getenv("RAG_STAGE_PROJECT")
		stageRequire(t, filepath.IsAbs(root), "absolute project path is required")
		dist := filepath.Join(root, "frontend", "dist")
		_, err := os.Stat(filepath.Join(dist, "index.html"))
		stageMust(t, err)

		engine.Static("/assets", filepath.Join(dist, "assets"))
		engine.NoRoute(func(c *gin.Context) {
			if c.Request.Method == http.MethodGet &&
				(c.Request.URL.Path == "/chat" || c.Request.URL.Path == "/login") {
				c.Status(http.StatusOK)
				c.File(filepath.Join(dist, "index.html"))
				return
			}
			c.Status(http.StatusNotFound)
		})
	}

	server := httptest.NewServer(engine)
	t.Cleanup(server.Close)
	client := server.Client()
	client.Timeout = 45 * time.Second
	t.Cleanup(client.CloseIdleConnections)

	stageSave(t, t.Name()+"-fixture.json", map[string]any{
		"fixture_kind": "synthetic_not_official",
		"question":     stageQuestion, "article_content": stageArticle,
		"candidate": candidate,
		"model":     cfg.Model, "base_url": cfg.BaseURL,
		"model_timeout":     timeout.String(),
		"candidate_sources": "controlled; real HybridService and MySQL hydration",
		"redis":             "not used by this isolated harness",
	})

	return &stageApp{
		db: db, server: server, client: client, token: token,
		user: user, candidate: candidate, generator: generator,
	}
}

func stageConversation(t *testing.T, app *stageApp, title string) uint64 {
	t.Helper()
	value := model.Conversation{UserID: app.user.ID, Title: title}
	stageMust(t, repository.NewConversationRepository(app.db).
		Create(context.Background(), &value))
	return value.ID
}

func stageHTTP(
	t *testing.T,
	app *stageApp,
	method, path, label string,
	body any,
	wantStatus int,
) ([]byte, http.Header) {
	t.Helper()
	var encoded []byte
	var err error
	if body != nil {
		encoded, err = json.Marshal(body)
		stageMust(t, err)
	}

	request, err := http.NewRequest(
		method, app.server.URL+path, bytes.NewReader(encoded),
	)
	stageMust(t, err)
	request.Header.Set("Authorization", "Bearer "+app.token)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}

	response, err := app.client.Do(request)
	stageMust(t, err)
	defer response.Body.Close()

	data, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	stageMust(t, err)
	stageRequire(t, len(data) <= 1<<20, "HTTP response exceeds test evidence limit")

	stageSave(t, label+"-http.json", map[string]any{
		"method": method, "path": path, "status": response.StatusCode,
		"content_type":  response.Header.Get("Content-Type"),
		"cache_control": response.Header.Get("Cache-Control"),
		"body":          string(data),
	})
	stageRequire(t, response.StatusCode == wantStatus,
		"unexpected HTTP status; inspect the saved response")
	return data, response.Header
}

func stageCheckCitation(
	t *testing.T,
	items model.MessageCitations,
	candidate retrieval.Candidate,
) {
	t.Helper()
	stageRequire(t, len(items) == 1, "expected exactly one grounded citation")
	c := items[0]
	r := candidate.Chunk
	stageRequire(t,
		c.RefID == "R1" &&
			c.DocumentID == strconv.FormatUint(r.DocumentID, 10) &&
			c.VersionID == strconv.FormatUint(r.VersionID, 10) &&
			c.ArticleID == strconv.FormatUint(r.ArticleID, 10) &&
			c.CategoryID == strconv.FormatUint(r.CategoryID, 10) &&
			c.ChunkID == strconv.FormatUint(r.ChunkID, 10) &&
			reflect.DeepEqual(c.ChunkIDs, []string{c.ChunkID}) &&
			c.Title == r.Title && c.Version == r.Version &&
			c.SectionPath == r.SectionPath &&
			c.ArticleNumber == r.ArticleNumber &&
			c.SourceURL == r.SourceURL &&
			c.EffectiveAt.Equal(r.EffectiveAt) &&
			c.ExpiresAt == nil && !c.RetrievedAt.IsZero(),
		"saved citation differs from the actual isolated rule",
	)
}

type stageEvent struct {
	Name string
	Data json.RawMessage
}

func stageEvents(t *testing.T, body []byte) []stageEvent {
	t.Helper()
	text := strings.ReplaceAll(string(body), "\r\n", "\n")
	var events []stageEvent
	for _, block := range strings.Split(strings.TrimSpace(text), "\n\n") {
		var event stageEvent
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(line, "event:") {
				event.Name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			}
			if strings.HasPrefix(line, "data:") {
				event.Data = json.RawMessage(
					strings.TrimSpace(strings.TrimPrefix(line, "data:")),
				)
			}
		}
		stageRequire(t, event.Name != "" && json.Valid(event.Data),
			"invalid SSE frame in actual response")
		events = append(events, event)
	}
	return events
}

func stageRequireSameMessages(
	t *testing.T,
	got, want []model.Message,
	label string,
) {
	t.Helper()
	stageRequire(t, len(got) == len(want),
		label+": message count differs")

	for i := range want {
		actual, expected := got[i], want[i]
		prefix := fmt.Sprintf("%s: message[%d]", label, i)

		stageRequire(t,
			actual.ID == expected.ID &&
				actual.ConversationID == expected.ConversationID &&
				actual.Role == expected.Role &&
				actual.Content == expected.Content,
			prefix+": identity, role or content differs",
		)
		stageRequire(t, actual.CreatedAt.Equal(expected.CreatedAt),
			prefix+": created_at differs")

		actualCitations, err := json.Marshal(actual.Citations)
		stageMust(t, err)
		expectedCitations, err := json.Marshal(expected.Citations)
		stageMust(t, err)

		stageRequire(t, bytes.Equal(actualCitations, expectedCitations),
			prefix+": citation snapshot differs")
	}
}

func TestRAGModelMySQL(t *testing.T) {
	app := stageLiveApp(t, false)
	messages := repository.NewMessageRepository(app.db)
	var original []model.Message
	var originalPath string

	for _, mode := range []string{"ordinary", "sse"} {
		if !t.Run(mode, func(t *testing.T) {
			conversationID := stageConversation(t, app, "【虚构测试】"+mode)
			path := fmt.Sprintf("/api/v1/conversations/%d/messages", conversationID)
			requestPath := path
			status := http.StatusCreated
			if mode == "sse" {
				requestPath += "/stream"
				status = http.StatusOK
			}

			beforeCalls := app.generator.calls.Load()
			data, headers := stageHTTP(
				t, app, http.MethodPost, requestPath, mode,
				map[string]string{"content": stageQuestion}, status,
			)
			stageRequire(t, app.generator.calls.Load() == beforeCalls+1,
				"each answer must use exactly one real model call")

			stored, err := messages.ListByConversationID(
				context.Background(), conversationID,
			)
			stageMust(t, err)
			stageRequire(t,
				len(stored) == 2 &&
					stored[0].Role == "user" &&
					stored[0].Content == stageQuestion &&
					len(stored[0].Citations) == 0 &&
					stored[1].Role == "assistant" &&
					stored[1].Content != "" &&
					stored[1].ID > stored[0].ID,
				"actual SQL pair differs from the expected completed turn",
			)
			stageCheckCitation(t, stored[1].Citations, app.candidate)
			stageRequire(t,
				strings.Contains(stored[1].Content, "[R1]") &&
					strings.Contains(stored[1].Content, app.candidate.Chunk.Title) &&
					strings.Contains(stored[1].Content, app.candidate.Chunk.SourceURL),
				"server-rendered answer is missing citation metadata",
			)

			if mode == "ordinary" {
				var returned model.Message
				stageMust(t, json.Unmarshal(data, &returned))
				stageRequire(t,
					returned.ID == stored[0].ID &&
						returned.Role == "user" &&
						returned.Content == stageQuestion &&
						len(returned.Citations) == 0,
					"ordinary endpoint must still return the saved user message",
				)
				original = stored
				originalPath = path
			} else {
				stageRequire(t,
					strings.HasPrefix(headers.Get("Content-Type"), "text/event-stream"),
					"stream response has the wrong content type",
				)
				events := stageEvents(t, data)
				names := make([]string, len(events))
				for i := range events {
					names[i] = events[i].Name
				}
				stageRequire(t,
					reflect.DeepEqual(names, []string{"start", "token", "citation", "done"}),
					"grounded SSE must emit start, one body, citation, then done",
				)

				var start struct {
					ConversationID uint64 `json:"conversation_id"`
				}
				var token struct {
					Content string `json:"content"`
				}
				var citations struct {
					Items model.MessageCitations `json:"items"`
				}
				var done struct {
					UserID      uint64 `json:"user_message_id"`
					AssistantID uint64 `json:"assistant_message_id"`
				}
				stageMust(t, json.Unmarshal(events[0].Data, &start))
				stageMust(t, json.Unmarshal(events[1].Data, &token))
				stageMust(t, json.Unmarshal(events[2].Data, &citations))
				stageMust(t, json.Unmarshal(events[3].Data, &done))

				stageRequire(t,
					start.ConversationID == conversationID &&
						token.Content == stored[1].Content &&
						reflect.DeepEqual(citations.Items, stored[1].Citations) &&
						done.UserID == stored[0].ID &&
						done.AssistantID == stored[1].ID,
					"SSE values do not match committed SQL messages",
				)
			}

			historyBody, _ := stageHTTP(
				t, app, http.MethodGet, path, mode+"-history", nil, http.StatusOK,
			)
			stageSave(t, mode+"-sql-messages.json", stored)

			var history []model.Message
			stageMust(t, json.Unmarshal(historyBody, &history))
			stageRequireSameMessages(t, history, stored,
				"HTTP history differs from committed SQL messages")
		}) {
			return
		}
	}

	t.Run("history_state", func(t *testing.T) {
		c := app.candidate.Chunk
		detailPath := fmt.Sprintf(
			"/api/v1/knowledge/versions/%d/articles/%d", c.VersionID, c.ArticleID,
		)

		checkDetail := func(label string, withdrawn bool) {
			body, headers := stageHTTP(
				t, app, http.MethodGet, detailPath, label, nil, http.StatusOK,
			)
			var detail struct {
				Identity map[string]string `json:"identity"`
				Article  struct {
					Content string `json:"content"`
				} `json:"article"`
				State map[string]bool `json:"state"`
			}
			stageMust(t, json.Unmarshal(body, &detail))
			wantIdentity := map[string]string{
				"document_id": strconv.FormatUint(c.DocumentID, 10),
				"version_id":  strconv.FormatUint(c.VersionID, 10),
				"article_id":  strconv.FormatUint(c.ArticleID, 10),
				"category_id": strconv.FormatUint(c.CategoryID, 10),
			}
			wantState := map[string]bool{
				"is_current":           !withdrawn,
				"is_superseded":        false,
				"is_expired":           false,
				"is_withdrawn":         withdrawn,
				"is_current_effective": !withdrawn,
			}
			stageRequire(t,
				headers.Get("Cache-Control") == "private, no-store" &&
					reflect.DeepEqual(detail.Identity, wantIdentity) &&
					reflect.DeepEqual(detail.State, wantState) &&
					detail.Article.Content == stageArticle,
				"historical identity, body or state differs",
			)
		}

		checkDetail("history-state-current", false)

		// 仅修改本次随机库内明确标记的虚构规则。
		now := time.Now().UTC().Truncate(time.Millisecond)
		stageMust(t, app.db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Model(&model.RuleVersion{}).
				Where("id=?", c.VersionID).
				Updates(map[string]any{
					"status":       model.RuleVersionStatusWithdrawn,
					"withdrawn_at": now,
				}).Error; err != nil {
				return err
			}
			return tx.Model(&model.KnowledgeDocument{}).
				Where("id=?", c.DocumentID).
				Updates(map[string]any{
					"current_version_id":   nil,
					"publication_revision": 2,
				}).Error
		}))

		checkDetail("history-state-withdrawn", true)

		historyBody, _ := stageHTTP(
			t, app, http.MethodGet, originalPath,
			"history-after-withdrawal", nil, http.StatusOK,
		)
		var historical []model.Message
		stageMust(t, json.Unmarshal(historyBody, &historical))
		stageRequireSameMessages(t, historical, original,
			"rule state change rewrote saved answer citations")

		// 候选仍返回旧条款，但真实 SQL 回表应排除已下线版本。
		conversationID := stageConversation(t, app, "【虚构测试】无当前依据")
		path := fmt.Sprintf("/api/v1/conversations/%d/messages", conversationID)
		beforeCalls := app.generator.calls.Load()
		stageHTTP(t, app, http.MethodPost, path, "withdrawn-no-evidence",
			map[string]string{"content": stageQuestion}, http.StatusCreated)
		stageRequire(t, app.generator.calls.Load() == beforeCalls,
			"no current evidence must not call the model")

		stored, err := messages.ListByConversationID(
			context.Background(), conversationID,
		)
		stageMust(t, err)
		stageRequire(t,
			len(stored) == 2 && stored[1].Content != "" &&
				len(stored[1].Citations) == 0,
			"no-evidence response must have no citations",
		)
		stageSave(t, "withdrawn-no-evidence-sql.json", stored)
	})

	stageRequire(t, app.generator.calls.Load() == 2,
		"automatic mode must make exactly two real model calls")
}

func TestRAGBrowserSession(t *testing.T) {
	stageRequire(t, os.Getenv("RAG_STAGE_PHASE") == "Browser",
		"browser session requires the Browser script phase")
	app := stageLiveApp(t, true)

	stageSave(t, "browser-ready.json", map[string]any{
		"url":          app.server.URL + "/chat",
		"fixture_kind": "synthetic_not_official",
	})
	t.Logf("BROWSER_URL=%s/chat", app.server.URL)

	stopPath := filepath.Join(os.Getenv("RAG_STAGE_EVIDENCE"), "browser-stop.txt")
	deadline := time.NewTimer(30 * time.Minute)
	defer deadline.Stop()
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()

wait:
	for {
		select {
		case <-deadline.C:
			t.Fatal("browser observation period expired")
		case <-ticker.C:
			if _, err := os.Stat(stopPath); err == nil {
				break wait
			} else if !os.IsNotExist(err) {
				stageMust(t, err)
			}
		}
	}

	var saved []model.Message
	stageMust(t, app.db.Order("id ASC").Find(&saved).Error)
	var citedAssistants int
	for _, message := range saved {
		if message.Role == "assistant" && len(message.Citations) != 0 {
			stageCheckCitation(t, message.Citations, app.candidate)
			citedAssistants++
		}
	}
	stageRequire(t,
		len(saved) >= 2 && citedAssistants >= 1 &&
			app.generator.calls.Load() >= 1,
		"browser session did not produce a real saved cited answer",
	)

	stageSave(t, "browser-machine.json", map[string]any{
		"messages":         saved,
		"cited_assistants": citedAssistants,
		"real_model_calls": app.generator.calls.Load(),
		"manual_observations_require_separate_review": true,
	})
}
