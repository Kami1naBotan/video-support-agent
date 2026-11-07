//go:build evaluationlive

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"video-support-agent/internal/config"
	"video-support-agent/internal/elasticsearch"
	"video-support-agent/internal/embedding"
	"video-support-agent/internal/evaluation"
	"video-support-agent/internal/knowledgeworker"
	"video-support-agent/internal/milvus"
	"video-support-agent/internal/model"
	"video-support-agent/internal/observability"
	"video-support-agent/internal/repository"
	"video-support-agent/internal/retrieval"
)

const liveModelDigest = "7907646426070047a77226ac3e684fbbe8410524f7b4a74d02837e43f2146bab"

func liveMust(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func liveSave(t *testing.T, path string, v any) {
	t.Helper()
	_, err := evaluation.WriteNewJSON(path, v)
	liveMust(t, err)
}
func liveRead(t *testing.T, path string, v any) {
	t.Helper()
	_, err := evaluation.ReadStrictJSON(path, v)
	liveMust(t, err)
}
func ownedDocker(t *testing.T, action, id string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	b, err := exec.CommandContext(ctx, "docker", "inspect", "--format", `{{index .Config.Labels "com.docker.compose.project"}}`, id).Output()
	liveMust(t, err)
	if strings.TrimSpace(string(b)) != os.Getenv("DUAL_TEST_PROJECT") || !strings.HasPrefix(os.Getenv("DUAL_TEST_PROJECT"), "evaluation-live-") {
		t.Fatal("container ownership mismatch")
	}
	if action != "inspect" {
		b, err = exec.CommandContext(ctx, "docker", action, id).CombinedOutput()
		if err != nil {
			t.Fatalf("owned container action: %s %v", b, err)
		}
	}
}

func prepareLiveEvaluation(t *testing.T) (string, string, *gorm.DB, liveConfig, *evaluation.Corpus, *evaluation.CorpusBinding, evaluation.CorpusManifest, []evaluation.ArticleBinding) {
	return prepareLiveEvaluationWithSource(t, "https://example.invalid/synthetic/")
}

func prepareLiveEvaluationWithSource(t *testing.T, sourceBase string) (string, string, *gorm.DB, liveConfig, *evaluation.Corpus, *evaluation.CorpusBinding, evaluation.CorpusManifest, []evaluation.ArticleBinding) {
	root, evidence := os.Getenv("EVALUATION_ROOT"), os.Getenv("EVALUATION_EVIDENCE")
	if os.Getenv("TEST_ISOLATED") != "1" || os.Getenv("DB_NAME") != "eval_retrieval_test" || !filepath.IsAbs(root) || !filepath.IsAbs(evidence) {
		t.Fatal("isolated evaluation script required")
	}
	for _, key := range []string{"ES", "MILVUS"} {
		ownedDocker(t, "inspect", os.Getenv("DUAL_TEST_"+key+"_CONTAINER"))
	}
	d := driver.NewConfig()
	d.Net = "tcp"
	d.Addr = net.JoinHostPort("127.0.0.1", os.Getenv("DB_PORT"))
	d.User = os.Getenv("DB_USER")
	d.Passwd = os.Getenv("DB_PASSWORD")
	d.DBName = os.Getenv("DB_NAME")
	d.ParseTime = true
	d.Timeout = 5 * time.Second
	d.ReadTimeout = 10 * time.Second
	d.WriteTimeout = 10 * time.Second
	db, err := gorm.Open(mysql.Open(d.FormatDSN()), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	liveMust(t, err)
	pool, err := db.DB()
	liveMust(t, err)
	t.Cleanup(func() { pool.Close() })
	var identity struct{ Name, UUID string }
	liveMust(t, db.Raw("SELECT DATABASE() AS name, @@server_uuid AS uuid").Scan(&identity).Error)
	if identity.Name != d.DBName || identity.UUID != os.Getenv("TEST_MYSQL_SERVER_UUID") {
		t.Fatal("database identity mismatch")
	}
	var count int64
	liveMust(t, db.Model(&model.KnowledgeDocument{}).Count(&count).Error)
	if count != 0 {
		t.Fatal("fresh database required")
	}
	// Synthetic fixture only: bootstrap the real build repository after seeding authoritative rows.
	category := model.RuleCategory{Code: "synthetic-evaluation", Name: "合成评测"}
	liveMust(t, db.Create(&category).Error)
	manifest := evaluation.CorpusManifest{SchemaVersion: 1, CorpusID: "synthetic-live-v1", Synthetic: true, Articles: []evaluation.CorpusArticle{}}
	bindings := []evaluation.ArticleBinding{}
	effective := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i, contents := range [][]string{{"合成申诉规则：收到通知后七日内可以提出申诉。", "合成申诉规则：提交申诉时必须附上通知编号。"}, {"合成退款规则：批准退款后五日内到账。"}} {
		key := []string{"synthetic-appeal", "synthetic-refund"}[i]
		doc := model.KnowledgeDocument{DocumentKey: key, PublicationRevision: 1}
		liveMust(t, db.Create(&doc).Error)
		version := model.RuleVersion{DocumentID: doc.ID, CategoryID: category.ID, Version: "v1", ContentRevision: 1, Status: model.RuleVersionStatusPublished, PublishedAt: &effective, EffectiveAt: &effective, Title: key, Content: strings.Join(contents, "\n"), SourceURL: sourceBase + key}
		liveMust(t, db.Omit("Document", "Category").Create(&version).Error)
		liveMust(t, db.Model(&doc).Update("current_version_id", version.ID).Error)
		for j, content := range contents {
			article := model.RuleArticle{VersionID: version.ID, ArticleOrder: uint32(j + 1), SectionPath: "合成测试", ArticleNumber: fmt.Sprint(j + 1), Content: content}
			liveMust(t, db.Omit("Version").Create(&article).Error)
			citation := evaluation.CitationKey{DocumentKey: key, VersionKey: "v1", ArticleKey: fmt.Sprintf("article-%d", j+1)}
			manifest.Articles = append(manifest.Articles, evaluation.CorpusArticle{Key: citation, CategoryKey: category.Code, Title: version.Title, Content: content, Scope: "仅用于隔离测试，不是官方政策", SourceURL: version.SourceURL, EffectiveAt: effective.Format(time.RFC3339)})
			bindings = append(bindings, evaluation.ArticleBinding{Key: citation, IDs: evaluation.RuntimeArticleID{DocumentID: doc.ID, VersionID: version.ID, ArticleID: article.ID}, CategoryID: category.ID})
		}
	}
	corpus, err := evaluation.NewCorpus(manifest)
	liveMust(t, err)
	bound, err := evaluation.BindCorpus(context.Background(), db, corpus, bindings)
	liveMust(t, err)
	t.Run("binding_rejects_wrong_parent_and_content", func(t *testing.T) {
		bad := append([]evaluation.ArticleBinding(nil), bindings...)
		bad[0].IDs.DocumentID = bindings[2].IDs.DocumentID
		if _, err := evaluation.BindCorpus(context.Background(), db, corpus, bad); err == nil {
			t.Fatal("wrong parent accepted")
		}
		changed := manifest
		changed.Articles = append([]evaluation.CorpusArticle(nil), manifest.Articles...)
		changed.Articles[0].Content += "篡改"
		altered, err := evaluation.NewCorpus(changed)
		liveMust(t, err)
		if _, err = evaluation.BindCorpus(context.Background(), db, altered, bindings); err == nil {
			t.Fatal("content mismatch accepted")
		}
	})
	cfg := liveConfig{SchemaVersion: 1, RunIndex: 1, Mode: "retrieval", Database: evaluationDatabase{Host: "127.0.0.1", Port: os.Getenv("DB_PORT"), User: d.User, Name: d.DBName, ServerUUID: identity.UUID}, Elasticsearch: config.ElasticsearchConfig{URL: os.Getenv("DUAL_TEST_ES_URL"), IndexAlias: "rule_chunks_current", Timeout: "5s"}, Milvus: config.MilvusConfig{URL: os.Getenv("DUAL_TEST_MILVUS_URL"), Database: "default", CollectionAlias: "rule_chunks_vector_current", Timeout: "5s"}, Embedding: config.EmbeddingConfig{URL: os.Getenv("EVALUATION_EMBEDDING_URL"), Model: embedding.ModelName, ModelDigest: liveModelDigest, Dimensions: "1024", ContextLength: "8192", Timeout: "60s"}}
	es, err := elasticsearch.NewClient(cfg.Elasticsearch)
	liveMust(t, err)
	defer es.Close()
	vector, err := milvus.NewClient(cfg.Milvus)
	liveMust(t, err)
	defer vector.Close()
	embed, err := embedding.NewClient(cfg.Embedding)
	liveMust(t, err)
	defer embed.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	modelIdentity, err := embed.InspectIdentity(ctx)
	liveMust(t, err)
	liveSave(t, filepath.Join(evidence, "embedding-identity.json"), modelIdentity)
	definition, err := os.ReadFile(filepath.Join(root, "deploy/elasticsearch/rule-chunks-v1.json"))
	liveMust(t, err)
	repo, err := repository.NewKnowledgeBuildRepository(db, 100)
	liveMust(t, err)
	builder, err := knowledgeworker.NewDualBuilder(repo, embed, es, vector, knowledgeworker.DualBuildOptions{ExpectedDatabase: d.DBName, ExpectedServerUUID: identity.UUID, ExpectedDigest: liveModelDigest, ESDefinition: definition})
	liveMust(t, err)
	claim, err := repo.ClaimLatest(ctx)
	liveMust(t, err)
	if claim == nil {
		t.Fatal("bootstrap claim missing")
	}
	verified, err := builder.Build(ctx, *claim)
	liveMust(t, err)
	liveMust(t, repo.Publish(ctx, verified))
	cfg.GenerationID = claim.GenerationID
	liveSave(t, filepath.Join(evidence, "published-generation.json"), verified)
	return root, evidence, db, cfg, corpus, bound, manifest, bindings
}

func TestLiveRetrievalEvaluation(t *testing.T) {
	root, evidence, _, cfg, corpus, bound, manifest, bindings := prepareLiveEvaluation(t)
	t.Run("real_search_observation", func(t *testing.T) {
		recorder := tracetest.NewSpanRecorder()
		provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
		defer provider.Shutdown(context.Background())
		runtime := &observability.Runtime{Enabled: true, Provider: provider, Registry: prometheus.NewRegistry()}
		esObs, err := observability.NewQueryObserver(runtime, "elasticsearch")
		require.NoError(t, err)
		mvObs, err := observability.NewQueryObserver(runtime, "milvus")
		require.NoError(t, err)
		es, err := elasticsearch.NewClient(cfg.Elasticsearch, esObs)
		require.NoError(t, err)
		defer es.Close()
		mv, err := milvus.NewClient(cfg.Milvus, mvObs)
		require.NoError(t, err)
		defer mv.Close()
		embed, err := embedding.NewClient(cfg.Embedding)
		require.NoError(t, err)
		defer embed.Close()
		searcher, err := milvus.NewSearcher(embed, mv, 35*time.Second)
		require.NoError(t, err)
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		identity, err := embed.InspectIdentity(ctx)
		require.NoError(t, err)
		suffix := strings.ReplaceAll(cfg.GenerationID, "-", "")
		at := time.Now().UTC().Truncate(time.Millisecond)
		rows, err := es.SearchCandidatesAt(ctx, "knowledge_g_"+suffix, "申诉期限", retrieval.SearchOptions{Limit: 3}, at)
		require.NoError(t, err)
		require.NotEmpty(t, rows)
		rows, err = searcher.SearchCandidatesAt(ctx, "rule_chunks_vec_v1_"+suffix, identity, "申诉期限", retrieval.SearchOptions{Limit: 3}, at)
		require.NoError(t, err)
		require.NotEmpty(t, rows)
		require.Len(t, recorder.Ended(), 2)
		families, err := runtime.Registry.Gather()
		require.NoError(t, err)
		require.Len(t, families, 2)
		for _, family := range families {
			require.Len(t, family.Metric, 1)
			require.EqualValues(t, 1, family.Metric[0].GetHistogram().GetSampleCount())
			require.Equal(t, "success", family.Metric[0].Label[0].GetValue())
		}
		liveSave(t, filepath.Join(evidence, "real-search-observation.json"), map[string]any{"spans": 2, "metrics": families, "scope": "real ES and Milvus query clients; in-memory span recorder, not OTLP"})
	})
	var err error
	cases := []evaluation.Case{}
	for i, question := range []string{"申诉期限和提交材料是什么？", "退款到账需要多久？", "申诉期限与退款到账期限分别是多少？", "合成规则是否规定火星航班时刻？"} {
		expected := [][]string{{"synthetic-appeal"}, {"synthetic-refund"}, {"synthetic-appeal", "synthetic-refund"}, {}}[i]
		c := evaluation.Case{ID: fmt.Sprintf("live-case-%d", i+1), SchemaVersion: 1, Suite: evaluation.SuiteRetrieval, Question: question, CorpusID: manifest.CorpusID, Filters: &evaluation.Filters{}, TopK: 3, ExpectedIntent: "retrieve", ExpectedDocuments: expected, ExpectedTools: []evaluation.ToolExpectation{}, ReferenceAnswer: "按合成语料核对；未提供的信息不得编造。", ExpectedCitations: []evaluation.CitationKey{}, RequiredPoints: []string{}, ForbiddenClaims: []string{"冒充官方政策"}}
		if i == 0 {
			c.Filters.DocumentKey = "synthetic-appeal"
		}
		if i == 1 {
			c.Filters.DocumentKey = "synthetic-refund"
		}
		liveMust(t, evaluation.ValidateCase(c))
		cases = append(cases, c)
	}
	material := filepath.Join(evidence, "materials")
	liveMust(t, os.Mkdir(material, 0700))
	liveSave(t, filepath.Join(material, "corpus.json"), manifest)
	liveSave(t, filepath.Join(material, "bindings.json"), bindings)
	liveSave(t, filepath.Join(material, "scenarios.json"), []evaluation.AgentScenario{})
	var lines []byte
	for _, c := range cases {
		b, err := json.Marshal(c)
		liveMust(t, err)
		lines = append(lines, b...)
		lines = append(lines, '\n')
	}
	liveMust(t, os.WriteFile(filepath.Join(material, "cases.jsonl"), lines, 0600))
	cfg.CasesFile, err = filepath.Rel(root, filepath.Join(material, "cases.jsonl"))
	liveMust(t, err)
	cfg.CorpusFile, err = filepath.Rel(root, filepath.Join(material, "corpus.json"))
	liveMust(t, err)
	cfg.BindingsFile, err = filepath.Rel(root, filepath.Join(material, "bindings.json"))
	liveMust(t, err)
	cfg.ScenariosFile, err = filepath.Rel(root, filepath.Join(material, "scenarios.json"))
	liveMust(t, err)
	run := func(t *testing.T, suffix string) evaluation.FrozenReport {
		t.Helper()
		c := cfg
		c.RunID = filepath.Base(evidence) + "-" + suffix
		path := filepath.Join(material, suffix+".json")
		liveSave(t, path, c)
		commandCtx, stop := context.WithTimeout(context.Background(), 150*time.Second)
		defer stop()
		command := exec.CommandContext(commandCtx, os.Getenv("EVALUATION_BINARY"), "run", "-root", root, "-config", path)
		command.Dir = root
		output, err := command.CombinedOutput()
		liveMust(t, os.WriteFile(filepath.Join(evidence, suffix+".log"), output, 0600))
		liveMust(t, err)
		dir := filepath.Join(root, ".scratch/test/evidence", c.RunID)
		var finish liveFinish
		liveRead(t, filepath.Join(dir, "run-finish.json"), &finish)
		if finish.Status != "finished" || finish.SavedCases != len(cases) {
			t.Fatalf("incomplete receipt %+v", finish)
		}
		for name, want := range finish.Artifacts {
			raw, err := os.ReadFile(filepath.Join(dir, name))
			liveMust(t, err)
			if fileBytesDigest(raw) != want {
				t.Fatalf("artifact hash mismatch %s", name)
			}
		}
		var snapshot evaluation.ScoreSnapshot
		liveRead(t, filepath.Join(dir, "score-snapshot.json"), &snapshot)
		if snapshot.Identity.BindingSHA256 != bound.Digest() || snapshot.Identity.CorpusSHA256 != corpus.Digest() {
			t.Fatal("identity mismatch")
		}
		var report evaluation.FrozenReport
		liveRead(t, filepath.Join(dir, "report-unreviewed.json"), &report)
		rescore := filepath.Join(evidence, suffix+"-rescored.json")
		command = exec.CommandContext(commandCtx, os.Getenv("EVALUATION_BINARY"), "rescore", "-snapshot", filepath.Join(dir, "score-snapshot.json"), "-out", rescore)
		output, err = command.CombinedOutput()
		if err != nil {
			t.Fatalf("rescore failed: %v %s", err, output)
		}
		var again evaluation.FrozenReport
		liveRead(t, rescore, &again)
		if !reflect.DeepEqual(report, again) {
			t.Fatal("offline score differs")
		}
		r := report.Retrieval
		if r == nil || r.TotalCases != 4 || r.ApplicableCases != 3 || r.NegativeCases != 1 {
			t.Fatalf("wrong denominators %+v", r)
		}
		t.Logf("run=%s recall=%v mrr=%v failed=%d degraded=%d negative_with_results=%d", c.RunID, r.RecallAtK, r.MRRAtK, r.FailedCases, r.DegradedCases, r.NegativeWithResults)
		return report
	}
	if !t.Run("healthy_real_retrieval", func(t *testing.T) {
		r := run(t, "healthy").Retrieval
		if r.FailedCases != 0 || r.DegradedCases != 0 || r.RecallAtK == nil || *r.RecallAtK != 1 || r.MRRAtK == nil || *r.MRRAtK != 1 {
			t.Fatalf("unexpected healthy report %+v", r)
		}
		if r.Cases[0].ReturnedItemsAtK != 2 || r.Cases[0].HitDocuments != 1 {
			t.Fatal("same-document article positions not preserved")
		}
	}) {
		return
	}
	ownedDocker(t, "stop", os.Getenv("DUAL_TEST_ES_CONTAINER"))
	t.Run("es_down_vector_degradation", func(t *testing.T) {
		r := run(t, "degraded").Retrieval
		if r.FailedCases != 0 || r.DegradedCases != 4 {
			t.Fatalf("degradation hidden %+v", r)
		}
	})
	ownedDocker(t, "stop", os.Getenv("DUAL_TEST_MILVUS_CONTAINER"))
	t.Run("both_down_failure_denominator", func(t *testing.T) {
		r := run(t, "failed").Retrieval
		if r.FailedCases != 4 || r.RecallAtK == nil || *r.RecallAtK != 0 || r.MRRAtK == nil || *r.MRRAtK != 0 {
			t.Fatalf("failures omitted %+v", r)
		}
	})
}
