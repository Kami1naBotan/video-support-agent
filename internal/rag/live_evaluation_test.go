//go:build ragevaluation

package rag_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"video-support-agent/internal/config"
	"video-support-agent/internal/database"
	"video-support-agent/internal/elasticsearch"
	"video-support-agent/internal/embedding"
	"video-support-agent/internal/llm"
	"video-support-agent/internal/milvus"
	"video-support-agent/internal/model"
	"video-support-agent/internal/rag"
	"video-support-agent/internal/repository"
	"video-support-agent/internal/retrieval"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"go.uber.org/zap"
)

type tenQuestionCase struct {
	ID       string `json:"id"`
	Question string `json:"question"`
}

// 只记录真实检索结果，不替换或修改候选。
type tenQuestionRetriever struct {
	base  retrieval.HybridRetriever
	last  retrieval.HybridResult
	calls int
}

func (r *tenQuestionRetriever) Retrieve(
	ctx context.Context,
	query string,
	opts retrieval.HybridOptions,
) (retrieval.HybridResult, error) {
	r.calls++
	result, err := r.base.Retrieve(ctx, query, opts)
	r.last = result
	return result, err
}

// 记录模型实际返回正文，不记录密钥或底层错误正文。
type tenQuestionGenerator struct {
	base   llm.Generator
	calls  int
	text   string
	finish string
}

func (g *tenQuestionGenerator) Generate(
	ctx context.Context,
	messages []*schema.Message,
	options ...einomodel.Option,
) (*schema.Message, error) {
	g.calls++
	g.text, g.finish = "", ""
	result, err := g.base.Generate(ctx, messages, options...)
	if result != nil {
		g.text = result.Content
		if result.ResponseMeta != nil {
			g.finish = result.ResponseMeta.FinishReason
		}
	}
	return result, err
}

func tenQuestionMust(t *testing.T, err error, operation string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s failed; inspect configuration and retained artifacts", operation)
	}
}

func tenQuestionSave(t *testing.T, name string, value any) {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	tenQuestionMust(t, err, "encode artifact")
	path := filepath.Join(os.Getenv("RAG_EVAL_OUTPUT"), name+".json")
	tenQuestionMust(t, os.WriteFile(path, data, 0600), "save artifact")
}

func TestRAGTenQuestions(t *testing.T) {
	if os.Getenv("RAG_EVAL_RUN") != "1" ||
		!filepath.IsAbs(os.Getenv("RAG_EVAL_OUTPUT")) {
		t.Fatal("run through the evaluation script")
	}

	data, err := os.ReadFile(os.Getenv("RAG_EVAL_CASES"))
	tenQuestionMust(t, err, "read declared questions")
	var cases []tenQuestionCase
	data = bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})
	tenQuestionMust(t, json.Unmarshal(data, &cases), "decode questions")
	if len(cases) != 10 {
		t.Fatal("exactly ten declared questions are required")
	}
	for i, item := range cases {
		if item.ID != fmt.Sprintf("Q%02d", i+1) ||
			strings.TrimSpace(item.Question) == "" {
			t.Fatal("question IDs or text differ from the declaration")
		}
	}

	cfg := config.Load()
	if cfg.Database.Host != "127.0.0.1" ||
		cfg.Database.Port != "13309" ||
		cfg.Database.Name != "rule_search_demo" ||
		cfg.Database.User != "rule_demo" {
		t.Fatal("only the retained isolated rule demo database is allowed")
	}

	setupCtx, cancelSetup := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelSetup()

	db, err := database.OpenMySQL(cfg.Database)
	tenQuestionMust(t, err, "open retained demo database")
	sqlDB, err := db.DB()
	tenQuestionMust(t, err, "get connection pool")
	t.Cleanup(func() {
		if err := sqlDB.Close(); err != nil {
			t.Error("close connection pool failed")
		}
	})

	var identity struct {
		DatabaseName string
		ServerUUID   string
	}
	err = db.WithContext(setupCtx).
		Raw("SELECT DATABASE() AS database_name, @@server_uuid AS server_uuid").
		Scan(&identity).Error
	tenQuestionMust(t, err, "read database identity")
	if identity.DatabaseName != "rule_search_demo" ||
		identity.ServerUUID != "11943408-d446-11f1-a81f-d2cc0395fae3" {
		t.Fatal("retained database identity differs")
	}

	repo := repository.NewKnowledgeRepository(db)
	snapshot, err := repo.ExportCurrent(setupCtx)
	tenQuestionMust(t, err, "read current corpus")
	if snapshot == nil || len(snapshot.Documents) != 1 ||
		snapshot.SourceDatabase != identity.DatabaseName ||
		snapshot.SourceServerUUID != identity.ServerUUID {
		t.Fatal("expected the declared single-rule corpus")
	}
	a := snapshot.Documents[0]

	const articleText = "You acknowledge and understand that you may use your account to log in to all products under the Bilibili ecosystem."
	const sourceURL = "https://www.bilibili.com/blackboard/era/44ZJAVYNG9lkl24M.html#2"
	if a.Content != articleText || a.ArticleNumber != "2.8" ||
		a.SourceURL != sourceURL ||
		!strings.Contains(a.Title, "中国大陆以外用户") {
		t.Fatal("current corpus differs from declared article A")
	}

	var article struct {
		Content  string
		Verified bool
	}
	err = db.WithContext(setupCtx).Raw(`
		SELECT a.content,
		       EXISTS(
		           SELECT 1 FROM rule_verifications r
		           WHERE r.version_id=v.id
		             AND r.verified_content_revision=v.content_revision
		       ) AS verified
		FROM rule_articles a
		JOIN rule_versions v ON v.id=a.version_id
		WHERE a.id=? AND v.id=?
	`, a.ArticleID, a.VersionID).Scan(&article).Error
	tenQuestionMust(t, err, "read complete verified article")
	if article.Content != articleText || !article.Verified {
		t.Fatal("complete article or source-verification record differs")
	}

	categoryID, err := strconv.ParseUint(a.CategoryID, 10, 64)
	tenQuestionMust(t, err, "parse category identity")
	if categoryID == 0 {
		t.Fatal("category identity must be positive")
	}
	excludedDocument, excludedVersion := uint64(1), uint64(1)
	if a.DocumentID == "1" {
		excludedDocument = 2
	}
	if a.VersionID == "1" {
		excludedVersion = 2
	}

	es, err := elasticsearch.NewClient(cfg.Elasticsearch)
	tenQuestionMust(t, err, "initialize Elasticsearch")
	defer es.Close()
	embedder, err := embedding.NewClient(cfg.Embedding)
	tenQuestionMust(t, err, "initialize embedding")
	defer embedder.Close()
	vector, err := milvus.NewClient(cfg.Milvus)
	tenQuestionMust(t, err, "initialize Milvus")
	defer vector.Close()
	searcher, err := milvus.NewSearcher(embedder, vector, 8*time.Second)
	tenQuestionMust(t, err, "initialize vector searcher")
	hybrid, err := retrieval.NewHybridService(es, searcher, repo, nil)
	tenQuestionMust(t, err, "initialize hybrid retrieval")

	esTarget, err := es.GetAliasTarget(setupCtx)
	tenQuestionMust(t, err, "resolve retained ES alias")
	vectorTarget, err := vector.GetAliasTarget(setupCtx)
	tenQuestionMust(t, err, "resolve retained Milvus alias")
	if esTarget == "" || vectorTarget == "" {
		t.Fatal("retained search target is absent; no rebuild was attempted")
	}

	timeout, err := time.ParseDuration(cfg.LLM.Timeout)
	tenQuestionMust(t, err, "parse model timeout")
	realModel, err := llm.NewChatModel(setupCtx, cfg.LLM)
	tenQuestionMust(t, err, "initialize real generator")
	generator := &tenQuestionGenerator{base: realModel}

	tenQuestionSave(t, "source", map[string]any{
		"snapshot":               snapshot,
		"article_content":        article.Content,
		"es_alias":               cfg.Elasticsearch.IndexAlias,
		"es_target_at_start":     esTarget,
		"milvus_alias":           cfg.Milvus.CollectionAlias,
		"milvus_target_at_start": vectorTarget,
		"embedding_model":        cfg.Embedding.Model,
		"embedding_digest":       cfg.Embedding.ModelDigest,
		"generation_model":       cfg.LLM.Model,
		"scope":                  "single retained article; direct services, no browser or message writes",
	})

	for index, item := range cases {
		t.Run(item.ID, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
			defer cancel()

			opts := retrieval.HybridOptions{TopK: 5}
			switch item.ID {
			case "Q05":
				opts.CategoryID = categoryID
			case "Q06":
				opts.DocumentID = excludedDocument
			case "Q07":
				opts.VersionID = excludedVersion
			}
			retrievalOnly := index >= 4 && index <= 6
			trace := &tenQuestionRetriever{base: hybrid}
			beforeCalls := generator.calls
			generator.text, generator.finish = "", ""

			var reply model.GroundedReply
			var runErr error
			if retrievalOnly {
				_, runErr = trace.Retrieve(ctx, item.Question, opts)
			} else {
				responder, createErr := rag.NewResponder(trace, generator, timeout, zap.NewNop())
				tenQuestionMust(t, createErr, "initialize responder")
				reply, runErr = responder.ReplyGrounded(ctx, nil, item.Question)
			}

			problems := make([]string, 0)
			rank := 0
			for i, hit := range trace.last.Items {
				c := hit.Rule.Chunk
				matchesA := strconv.FormatUint(c.DocumentID, 10) == a.DocumentID &&
					strconv.FormatUint(c.VersionID, 10) == a.VersionID &&
					strconv.FormatUint(c.ArticleID, 10) == a.ArticleID
				if matchesA {
					rank = i + 1
				}
				if !matchesA || hit.Rule.ArticleContent != articleText ||
					c.SourceURL != sourceURL || c.ArticleNumber != "2.8" {
					problems = append(problems, "returned article differs from corpus A")
				}
				if item.ID == "Q05" && c.CategoryID != categoryID {
					problems = append(problems, "category filter mismatch")
				}
			}
			if runErr != nil {
				problems = append(problems, "retrieval or generation failed")
			}
			if trace.calls != 1 || len(trace.last.Items) > 5 {
				problems = append(problems, "retrieval count or TopK contract mismatch")
			}
			if trace.last.Degraded {
				problems = append(problems, "search dependency degraded during evaluation")
			}
			if index < 5 && rank == 0 {
				problems = append(problems, "expected article A was not in Top5")
			}
			if (item.ID == "Q06" || item.ID == "Q07") && len(trace.last.Items) != 0 {
				problems = append(problems, "excluded article passed the filter")
			}
			if index < 4 && reply.Decision != model.ReplyAnswer {
				problems = append(problems, "positive question did not produce an answer")
			}
			for _, citation := range reply.Citations {
				if citation.DocumentID != a.DocumentID ||
					citation.VersionID != a.VersionID ||
					citation.ArticleID != a.ArticleID ||
					citation.SourceURL != sourceURL {
					problems = append(problems, "answer citation identity mismatch")
				}
			}
			calls := generator.calls - beforeCalls
			if calls > 1 || (retrievalOnly && calls != 0) {
				problems = append(problems, "unexpected generation call count")
			}

			tenQuestionSave(t, item.ID, map[string]any{
				"id":                 item.ID,
				"question":           item.Question,
				"options":            opts,
				"retrieval":          trace.last,
				"article_a_rank":     rank,
				"retrieval_only":     retrievalOnly,
				"reply":              reply,
				"generation_calls":   calls,
				"raw_model_content":  generator.text,
				"finish_reason":      generator.finish,
				"operation_failed":   runErr != nil,
				"technical_problems": problems,
				"semantic_review":    "pending; compare with declared expectations",
			})
			for _, problem := range problems {
				t.Error(problem)
			}
		})
	}
}
