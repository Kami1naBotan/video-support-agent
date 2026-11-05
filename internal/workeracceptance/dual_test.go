//go:build workerdual

package workeracceptance

import (
	"context"
	"fmt"
	"gorm.io/gorm"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
	"video-support-agent/internal/config"
	"video-support-agent/internal/elasticsearch"
	"video-support-agent/internal/embedding"
	"video-support-agent/internal/knowledgeevent"
	"video-support-agent/internal/knowledgeworker"
	"video-support-agent/internal/milvus"
	"video-support-agent/internal/model"
	"video-support-agent/internal/repository"
	"video-support-agent/internal/retrieval"
)

const pinnedDigest = "7907646426070047a77226ac3e684fbbe8410524f7b4a74d02837e43f2146bab"

type controlledEmbedding struct{}

func (controlledEmbedding) InspectIdentity(context.Context) (embedding.ModelIdentity, error) {
	return embedding.ModelIdentity{Model: embedding.ModelName, Digest: pinnedDigest, Dimensions: embedding.Dimensions, ContextLength: embedding.ContextLength, InputPolicy: embedding.InputPolicy, ServerVersion: "controlled-test"}, nil
}
func (e controlledEmbedding) EmbedWithIdentity(ctx context.Context, texts []string) (embedding.BatchResult, error) {
	id, _ := e.InspectIdentity(ctx)
	vectors := make([][]float32, len(texts))
	for i := range vectors {
		vectors[i] = make([]float32, embedding.Dimensions)
		vectors[i][0] = 1
	}
	return embedding.BatchResult{Identity: id, Vectors: vectors}, ctx.Err()
}

type dualFixture struct {
	db      *gorm.DB
	repo    *repository.KnowledgeBuildRepository
	es      *elasticsearch.Client
	vector  *milvus.Client
	options knowledgeworker.DualBuildOptions
}

func newDual(t *testing.T) *dualFixture {
	t.Helper()
	project := os.Getenv("DUAL_TEST_PROJECT")
	if !strings.HasPrefix(project, "worker-dual-") {
		t.Fatal("isolated indexes required")
	}
	for _, key := range []string{"ES", "MILVUS"} {
		id := os.Getenv("DUAL_TEST_" + key + "_CONTAINER")
		if id == "" || docker(t, "inspect", "--format", `{{index .Config.Labels "com.docker.compose.project"}}`, id) != project {
			t.Fatal("index ownership mismatch")
		}
		if !strings.HasPrefix(os.Getenv("DUAL_TEST_"+key+"_URL"), "http://127.0.0.1:") {
			t.Fatal("nonlocal index endpoint")
		}
	}
	db, repo := workerSQLFixture(t)
	es, e := elasticsearch.NewClient(config.ElasticsearchConfig{URL: os.Getenv("DUAL_TEST_ES_URL"), IndexAlias: "rule_chunks_current", Timeout: "10s"})
	must(t, e)
	t.Cleanup(es.Close)
	vector, e := milvus.NewClient(config.MilvusConfig{URL: os.Getenv("DUAL_TEST_MILVUS_URL"), Database: "default", CollectionAlias: "rule_chunks_vector_current", Timeout: "10s"})
	must(t, e)
	t.Cleanup(vector.Close)
	definition, e := os.ReadFile("../../deploy/elasticsearch/rule-chunks-v1.json")
	must(t, e)
	return &dualFixture{db: db, repo: repo, es: es, vector: vector, options: knowledgeworker.DualBuildOptions{ExpectedDatabase: "bili_support_test", ExpectedServerUUID: os.Getenv("TEST_MYSQL_SERVER_UUID"), ExpectedDigest: pinnedDigest, ESDefinition: definition}}
}
func (f *dualFixture) source(t *testing.T) knowledgeevent.Source {
	s := workerSQLSource(t, f.db)
	version, _ := strconv.ParseUint(s.VersionID, 10, 64)
	article := model.RuleArticle{VersionID: version, ArticleOrder: 1, SectionPath: "测试", ArticleNumber: "1", Content: "Synthetic recovery rule. 测试规则：知识库更新后应保持原始条款和检索片段一致。"}
	must(t, f.db.Omit("Version").Create(&article).Error)
	inbox, e := repository.NewKnowledgeInboxRepository(f.db)
	must(t, e)
	must(t, inbox.AcceptSource(context.Background(), workerSQLDelivery(t, s)))
	return s
}
func (f *dualFixture) build(t *testing.T, embedder knowledgeworker.IdentityEmbedder) *knowledgeworker.VerifiedGeneration {
	t.Helper()
	b, e := knowledgeworker.NewDualBuilder(f.repo, embedder, f.es, f.vector, f.options)
	must(t, e)
	ctx, c := context.WithTimeout(context.Background(), 2*time.Minute)
	defer c()
	claim, e := f.repo.ClaimLatest(ctx)
	must(t, e)
	if claim == nil {
		t.Fatal("no build claim")
	}
	result, e := b.Build(ctx, *claim)
	must(t, e)
	return result
}
func (f *dualFixture) verifyPublished(t *testing.T, v *knowledgeworker.VerifiedGeneration, embedder knowledgeworker.IdentityEmbedder) {
	t.Helper()
	ctx, c := context.WithTimeout(context.Background(), 30*time.Second)
	defer c()
	must(t, f.repo.Publish(ctx, v))
	resolver, e := repository.NewKnowledgeActiveRepository(f.db)
	must(t, e)
	target, e := resolver.ResolveActive(ctx)
	must(t, e)
	if target.GenerationID != v.Claim.GenerationID || target.ESIndex != v.Claim.ESIndexName || target.MilvusCollection != v.Claim.MilvusCollection || target.ManifestSHA256 != v.Snapshot.ManifestSHA256 {
		t.Fatal("active pair or manifest differs")
	}
	if len(v.Snapshot.Prepared.Documents) != 1 {
		t.Fatalf("expected one document, got %d", len(v.Snapshot.Prepared.Documents))
	}
	opts := retrieval.SearchOptions{Limit: 10}
	esHits, e := f.es.SearchCandidatesAt(ctx, target.ESIndex, "Synthetic", opts, time.Now().UTC())
	must(t, e)
	searcher, e := milvus.NewSearcher(embedder, f.vector, 10*time.Second)
	must(t, e)
	vectorHits, e := searcher.SearchCandidatesAt(ctx, target.MilvusCollection, target.Identity, "Synthetic", opts, time.Now().UTC())
	must(t, e)
	if len(esHits) != 1 || len(vectorHits) != 1 || esHits[0].Chunk.ChunkID != vectorHits[0].Chunk.ChunkID {
		t.Fatalf("query pair differs es=%d vector=%d", len(esHits), len(vectorHits))
	}
	var event model.KnowledgeEventInbox
	must(t, f.db.Take(&event).Error)
	if event.Status != model.KnowledgeEventApplied {
		t.Fatalf("event status=%s", event.Status)
	}
	workerSQLCount(t, f.db, &model.EventOutbox{}, 1)
	t.Logf("published generation=%s epoch=%d chunk=%v model=%s digest=%s es=%s milvus=%s manifest=%s", target.GenerationID, target.Epoch, esHits[0].Chunk.ChunkID, target.Identity.Model, target.Identity.Digest, target.ESIndex, target.MilvusCollection, target.ManifestSHA256)
}
func TestWorkerDualRoundTrip(t *testing.T) {
	f := newDual(t)
	f.source(t)
	e := controlledEmbedding{}
	v := f.build(t, e)
	f.verifyPublished(t, v, e)
}
func TestWorkerDualRealModel(t *testing.T) {
	f := newDual(t)
	f.source(t)
	e, err := embedding.NewClient(config.EmbeddingConfig{URL: "http://127.0.0.1:11434", Model: embedding.ModelName, ModelDigest: pinnedDigest, Dimensions: fmt.Sprint(embedding.Dimensions), ContextLength: fmt.Sprint(embedding.ContextLength), Timeout: "60s"})
	must(t, err)
	defer e.Close()
	v := f.build(t, e)
	f.verifyPublished(t, v, e)
}
