//go:build indexexportintegration && vectorimportintegration && vectorhydrationintegration && raghybridintegration

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
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"video-support-agent/internal/config"
	"video-support-agent/internal/elasticsearch"
	"video-support-agent/internal/embedding"
	indexing "video-support-agent/internal/index"
	"video-support-agent/internal/milvus"
	"video-support-agent/internal/model"
	"video-support-agent/internal/retrieval"
	"video-support-agent/internal/vectorrebuild"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const rglESUUID = "tdU75ixmQt--vEW3NG__NQ"
const rglQuery = "隔离检索"

func rglSave(t *testing.T, dir, name string, value any) {
	t.Helper()
	vectorLiveSave(t, dir, name, vectorLiveJSON(t, value))
}

func rglESGet(
	t *testing.T,
	client *http.Client,
	dir, label, path string,
	target any,
) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	request, err := http.NewRequestWithContext(
		ctx, http.MethodGet, "http://127.0.0.1:9200"+path, nil,
	)
	vhMust(t, err)
	response, err := client.Do(request)
	vhMust(t, err)
	defer response.Body.Close()

	body, err := io.ReadAll(io.LimitReader(response.Body, (4<<20)+1))
	vhMust(t, err)
	vectorLiveSave(t, dir, label+".json", body)
	rglSave(t, dir, label+"-status.json", response.StatusCode)

	if response.StatusCode != http.StatusOK || len(body) > 4<<20 {
		t.Fatalf("ES diagnostic request failed: %s status=%d",
			path, response.StatusCode)
	}
	vhMust(t, json.Unmarshal(body, target))
}

func rglESClient(t *testing.T, alias string) *elasticsearch.Client {
	t.Helper()
	client, err := elasticsearch.NewClient(config.ElasticsearchConfig{
		URL:        "http://127.0.0.1:9200",
		IndexAlias: alias,
		Timeout:    "5s",
	})
	vhMust(t, err)
	t.Cleanup(client.Close)
	return client
}

func rglMVClient(t *testing.T, alias string) *milvus.Client {
	t.Helper()
	client, err := milvus.NewClient(config.MilvusConfig{
		URL:             "http://127.0.0.1:19530",
		Database:        "default",
		CollectionAlias: alias,
		Timeout:         "60s",
	})
	vhMust(t, err)
	t.Cleanup(client.Close)
	return client
}

func rglProtected(
	t *testing.T,
	httpClient *http.Client,
	protectedES *elasticsearch.Client,
	vectorHTTP *vectorLiveHTTP,
	dir, phase string,
) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	target, err := protectedES.GetAliasTarget(ctx)
	vhMust(t, err)
	if target == "" {
		t.Fatal("established ES alias is missing")
	}

	var alias, mapping any
	var count struct {
		Count int64 `json:"count"`
	}
	rglESGet(t, httpClient, dir, phase+"-es-alias",
		"/_alias/rule_chunks_current", &alias)
	rglESGet(t, httpClient, dir, phase+"-es-mapping",
		"/"+target+"/_mapping", &mapping)
	rglESGet(t, httpClient, dir, phase+"-es-count",
		"/"+target+"/_count", &count)

	vectors := vectorHTTP.protectedSnapshot()
	vectors["s02_alias"] = vectorHTTP.call(
		"aliases/describe",
		map[string]any{"aliasName": "vec_import_it_999bd24a8ab257d7"},
	)
	vectors["s02_count"] = vectorHTTP.count(
		"rule_chunks_vec_v1_005160370c3e73bc51a75dcd80f4f009",
	)

	result := map[string]any{
		"es_target":  target,
		"es_alias":   alias,
		"es_mapping": mapping,
		"es_count":   count.Count,
		"milvus":     vectors,
	}
	rglSave(t, dir, phase+"-protected.json", result)
	return result
}

func rglService(
	t *testing.T,
	es, mv retrieval.CandidateSearcher,
	repo retrieval.CurrentHydrator,
	at time.Time,
) *retrieval.HybridService {
	t.Helper()
	service, err := retrieval.NewHybridServiceWithClock(
		es, mv, repo, nil, func() time.Time { return at },
	)
	vhMust(t, err)
	return service
}

func rglCall(
	t *testing.T,
	dir, label string,
	service retrieval.HybridRetriever,
	opts retrieval.HybridOptions,
) (retrieval.HybridResult, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	result, err := service.Retrieve(ctx, rglQuery, opts)
	rglSave(t, dir, label+".json", map[string]any{
		"query":     rglQuery,
		"options":   opts,
		"result":    result,
		"has_error": err != nil,
	})
	return result, err
}

func rglOptions(f exportMySQLFixture) retrieval.HybridOptions {
	return retrieval.HybridOptions{
		DocumentID: f.document.ID,
		VersionID:  f.version.ID,
		CategoryID: f.category.ID,
		TopK:       10,
	}
}

func rglBoth(t *testing.T, result retrieval.HybridResult, count int) {
	t.Helper()
	if result.Degraded {
		t.Fatal("real two-source request unexpectedly degraded")
	}
	for _, report := range result.Stats.Sources {
		wantStatus := retrieval.SourceSucceeded
		if count == 0 {
			wantStatus = retrieval.SourceEmpty
		}
		if report.Status != wantStatus ||
			report.ErrorKind != retrieval.SourceErrorNone ||
			!report.RawCountKnown || report.RawCount != count {
			t.Fatalf("source report differs: %+v", report)
		}
	}
}

func rglEmpty(
	t *testing.T,
	result retrieval.HybridResult,
	reason string,
) {
	t.Helper()
	if result.Items == nil || len(result.Items) != 0 ||
		result.Outcome != retrieval.HybridNoEvidence ||
		result.EmptyReason != reason ||
		result.Stats.FinalCount != 0 ||
		result.ErrorKind != "" {
		t.Fatalf("expected empty current evidence: %+v", result)
	}
	if result.Stats.Rerank.Status != retrieval.HybridStageSkipped {
		t.Fatal("empty evidence unexpectedly reached rerank")
	}
}

// 只用于本测试新建的虚构夹具；不调用正式发布接口。
func rglChangeCurrent(
	t *testing.T,
	s *exportMySQLSuite,
	f exportMySQLFixture,
	replacement bool,
) {
	t.Helper()
	vhMust(t, s.writer.Transaction(func(tx *gorm.DB) error {
		var current any
		if replacement {
			next := f.version
			next.ID = 0
			next.Version = "v2"
			next.ExpiresAt = nil
			if err := tx.Omit(clause.Associations).Create(&next).Error; err != nil {
				return err
			}
			current = next.ID
		} else {
			result := tx.Model(&model.RuleVersion{}).
				Where("id = ?", f.version.ID).
				Updates(map[string]any{
					"status":       "withdrawn",
					"withdrawn_at": s.at,
				})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return fmt.Errorf("fixture withdrawal affected %d rows",
					result.RowsAffected)
			}
		}

		result := tx.Model(&model.KnowledgeDocument{}).
			Where("id = ?", f.document.ID).
			Updates(map[string]any{
				"current_version_id":   current,
				"publication_revision": f.document.PublicationRevision + 1,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf("fixture publication affected %d rows",
				result.RowsAffected)
		}
		return nil
	}))
}

func TestRAGHybridLive(t *testing.T) {
	if os.Getenv("RUN_RAG_HYBRID_LIVE") != "1" ||
		os.Getenv("RAG_EXPECT_ES_UUID") != rglESUUID {
		t.Fatal("explicit RAG integration configuration is required")
	}
	root := os.Getenv("RAG_PROJECT_ROOT")
	dir := os.Getenv("RAG_LIVE_EVIDENCE")
	if !filepath.IsAbs(root) || !filepath.IsAbs(dir) {
		t.Fatal("absolute project and evidence paths are required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	httpClient := &http.Client{
		Timeout: 60 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	t.Cleanup(httpClient.CloseIdleConnections)

	var identity struct {
		ClusterUUID string `json:"cluster_uuid"`
		Version     struct {
			Number string `json:"number"`
		} `json:"version"`
	}
	rglESGet(t, httpClient, dir, "live-es-identity", "/", &identity)
	if identity.ClusterUUID != rglESUUID ||
		identity.Version.Number != "8.19.0" {
		t.Fatal("ES identity differs; no test resource created")
	}
	var health struct {
		Status   string `json:"status"`
		TimedOut bool   `json:"timed_out"`
	}
	rglESGet(t, httpClient, dir, "live-es-health",
		"/_cluster/health", &health)
	if health.TimedOut ||
		(health.Status != "yellow" && health.Status != "green") {
		t.Fatal("ES is not ready")
	}

	vectorHTTP := &vectorLiveHTTP{
		t:       t,
		dir:     dir,
		client:  httpClient,
		mutable: make(map[string]bool),
	}
	protectedES := rglESClient(t, "rule_chunks_current")
	protectedBefore := rglProtected(
		t, httpClient, protectedES, vectorHTTP, dir, "before",
	)
	t.Cleanup(func() {
		after := rglProtected(
			t, httpClient, protectedES, vectorHTTP, dir, "after",
		)
		if !reflect.DeepEqual(protectedBefore, after) {
			t.Error("established protected resources changed")
		}
	})

	embedder, err := embedding.NewClient(config.EmbeddingConfig{
		URL:           "http://127.0.0.1:11434",
		Model:         "bge-m3:567m",
		ModelDigest:   vectorLiveDigest,
		Dimensions:    "1024",
		ContextLength: "8192",
		Timeout:       "120s",
	})
	vhMust(t, err)
	t.Cleanup(embedder.Close)

	// 准备阶段预热，不计入后续每次 HybridService 的检索预算。
	warm, err := embedder.EmbedWithIdentity(
		ctx, []string{"【虚构测试】隔离检索模型准备"},
	)
	vhMust(t, err)
	if warm.Identity.ServerVersion != "0.40.13" ||
		warm.Identity.Digest != vectorLiveDigest ||
		len(warm.Vectors) != 1 ||
		len(warm.Vectors[0]) != 1024 {
		t.Fatal("embedding identity or dimensions differ")
	}
	rglSave(t, dir, "embedding-identity.json", warm.Identity)

	// 复用固定地址、UUID、MySQL版本与InnoDB守卫。
	s := vhSuite(t)
	s.db = s.db.WithContext(ctx)
	s.writer = s.writer.WithContext(ctx)
	s.at = time.Now().UTC().Truncate(time.Millisecond)

	names := []string{
		"primary", "revision", "replaced", "withdrawn",
		"expired", "category", "timed", "body",
	}
	fixtures := make(map[string]exportMySQLFixture, len(names))
	expectedBodies := make(map[uint64]string)
	expectedChunks := make(map[uint64]string)
	expectations := make([]map[string]any, 0, len(names))

	for _, name := range names {
		f := s.fixture(t)
		f.version.Title = "【虚构测试】隔离检索规则 " + name
		f.article.Content = "【虚构测试，不作为官方依据】隔离检索完整条款 " +
			name + "。完整正文还包含后续条件，不能用片段替代。"
		f.chunk.Content = "【虚构测试】隔离检索片段 " + name

		vhUpdate(t, s, "rule_versions", f.version.ID, "title", f.version.Title)
		vhUpdate(t, s, "rule_articles", f.article.ID, "content", f.article.Content)
		vhUpdate(t, s, "knowledge_chunks", f.chunk.ID, "content", f.chunk.Content)

		if name == "timed" {
			expires := s.at.Add(time.Hour)
			f.version.ExpiresAt = &expires
			vhUpdate(t, s, "rule_versions", f.version.ID, "expires_at", expires)
		}

		fixtures[name] = f
		expectedBodies[f.document.ID] = f.article.Content
		expectedChunks[f.chunk.ID] = f.chunk.Content
		expectations = append(expectations, map[string]any{
			"name":             name,
			"document_id":      f.document.ID,
			"version_id":       f.version.ID,
			"article_id":       f.article.ID,
			"category_id":      f.category.ID,
			"chunk_id":         f.chunk.ID,
			"article_content":  f.article.Content,
			"fragment_content": f.chunk.Content,
		})
	}

	primary := fixtures["primary"]
	second := model.KnowledgeChunk{
		ArticleID:  primary.article.ID,
		ChunkIndex: 1,
		Content:    "【虚构测试】隔离检索补充片段 primary",
	}
	vhMust(t, s.db.Omit(clause.Associations).Create(&second).Error)
	expectedChunks[second.ID] = second.Content

	rglSave(t, dir, "expectations.json", map[string]any{
		"fixture_kind":      "synthetic_not_official",
		"query":             rglQuery,
		"captured_at":       s.at,
		"articles":          expectations,
		"second_chunk_id":   second.ID,
		"expected_chunks":   9,
		"expected_articles": 8,
	})

	var random [16]byte
	_, err = rand.Read(random[:])
	vhMust(t, err)
	suffix := hex.EncodeToString(random[:])
	esAlias := "rag_es_it_" + suffix
	esIndex := "rag_chunks_it_" + suffix
	mvAlias := "rag_mv_it_" + suffix
	collection := "rule_chunks_vec_v1_" + suffix

	rglSave(t, dir, "resources-planned.json", map[string]any{
		"database_created":   s.name,
		"server_uuid":        s.uuid,
		"es_alias_planned":   esAlias,
		"es_index_planned":   esIndex,
		"mv_alias_planned":   mvAlias,
		"collection_planned": collection,
		"resources_retained": true,
	})

	esClient := rglESClient(t, esAlias)
	mvClient := rglMVClient(t, mvAlias)
	oldES, err := esClient.GetAliasTarget(ctx)
	vhMust(t, err)
	oldMV, err := mvClient.GetAliasTarget(ctx)
	vhMust(t, err)
	if oldES != "" || oldMV != "" {
		t.Fatal("random alias already exists; refusing to replace it")
	}

	repo := NewKnowledgeRepositoryWithClock(
		s.db, func() time.Time { return s.at },
	)
	sourceBefore, err := repo.ExportCurrent(ctx)
	vhMust(t, err)
	if len(sourceBefore.Documents) != 9 {
		t.Fatal("fixture export did not contain exactly nine chunks")
	}
	rglSave(t, dir, "source-before.json", sourceBefore)

	definition, err := os.ReadFile(filepath.Join(
		root, "deploy", "elasticsearch", "rule-chunks-v1.json",
	))
	vhMust(t, err)
	esResult, err := indexing.Rebuild(
		ctx, repo, esClient,
		indexing.RebuildOptions{
			NewIndex:           esIndex,
			Definition:         definition,
			ExpectedDatabase:   s.name,
			ExpectedServerUUID: s.uuid,
		},
	)
	rglSave(t, dir, "es-rebuild.json", esResult)
	vhMust(t, err)
	if esResult.Stage != "complete" ||
		esResult.DocumentCount != 9 ||
		esResult.OldIndex != "" ||
		esResult.Publication.State != indexing.PublicationApplied ||
		esResult.Publication.ObservedTarget != esIndex {
		t.Fatal("ES fixture publication was not confirmed")
	}

	mvResult, err := vectorrebuild.Rebuild(
		ctx, repo, embedder, mvClient,
		vectorrebuild.Options{
			NewCollection:      collection,
			ExpectedDatabase:   s.name,
			ExpectedServerUUID: s.uuid,
			ExpectedDigest:     vectorLiveDigest,
		},
		func(result vectorrebuild.Result) error {
			data, err := json.MarshalIndent(result, "", "  ")
			if err != nil {
				return err
			}
			return os.WriteFile(
				filepath.Join(dir, "vector-rebuild-"+result.Stage+".json"),
				data, 0600,
			)
		},
	)
	rglSave(t, dir, "vector-rebuild-final.json", mvResult)
	vhMust(t, err)
	if mvResult.Stage != "complete" || mvResult.Count != 9 ||
		mvResult.OldCollection != "" ||
		mvResult.Publication.State != milvus.PublicationApplied ||
		mvResult.Identity.ServerVersion != "0.40.13" {
		t.Fatal("Milvus fixture publication was not confirmed")
	}
	actualMV, err := mvClient.GetAliasTarget(ctx)
	vhMust(t, err)
	if actualMV != collection || vectorHTTP.count(collection) != 9 {
		t.Fatal("published Milvus target or count differs")
	}

	sourceAfter, err := repo.ExportCurrent(ctx)
	vhMust(t, err)
	rglSave(t, dir, "source-after-build.json", sourceAfter)
	if !reflect.DeepEqual(sourceBefore.Documents, sourceAfter.Documents) {
		t.Fatal("index construction changed the source rules")
	}
	rglSave(t, dir, "resources-ready.json", map[string]any{
		"database": s.name, "server_uuid": s.uuid,
		"es_alias": esAlias, "es_index": esIndex,
		"milvus_alias": mvAlias, "milvus_collection": collection,
		"chunks": 9, "resources_retained": true,
	})
	t.Logf("RAG_TEST_DATABASE=%s ES_INDEX=%s MILVUS_COLLECTION=%s",
		s.name, esIndex, collection)

	mvSearcher, err := milvus.NewSearcher(embedder, mvClient, 35*time.Second)
	vhMust(t, err)
	service := rglService(t, esClient, mvSearcher, repo, s.at)

	requireSubtest := func(name string, test func(*testing.T)) {
		t.Helper()
		if !t.Run(name, test) {
			t.Fatalf("integration checkpoint failed: %s; resources retained", name)
		}
	}

	requireSubtest("baseline", func(t *testing.T) {
		got, err := rglCall(
			t, dir, "baseline", service, retrieval.HybridOptions{TopK: 10},
		)
		vhMust(t, err)
		rglBoth(t, got, 9)
		if got.Outcome != retrieval.HybridSuccess ||
			len(got.Items) != 8 ||
			got.Stats.RawCount != 18 ||
			got.Stats.DuplicateCount != 9 ||
			got.Stats.ConflictCount != 0 ||
			got.Stats.UniqueCount != 9 ||
			got.Stats.ClippedCount != 0 ||
			got.Stats.FusedCount != 9 ||
			got.Stats.HydratedCount != 9 ||
			got.Stats.FilteredCount != 0 ||
			got.Stats.ArticleCount != 8 ||
			got.Stats.FinalCount != 8 ||
			got.Stats.Rerank.Status != retrieval.HybridStageSkipped {
			t.Fatalf("real baseline stats differ: %+v", got.Stats)
		}

		seen := make(map[uint64]bool)
		for _, item := range got.Items {
			chunk := item.Rule.Chunk
			wantBody, exists := expectedBodies[chunk.DocumentID]
			if !exists || seen[chunk.DocumentID] ||
				item.Rule.ArticleContent != wantBody ||
				chunk.Content != expectedChunks[chunk.ChunkID] ||
				chunk.Content == item.Rule.ArticleContent ||
				chunk.SourceURL != "https://example.invalid/isolated-rule" ||
				chunk.ArticleNumber != "第一条" ||
				chunk.Score != item.RRFScore ||
				len(chunk.Highlights) != 0 {
				t.Fatal("authoritative article or reference fields differ")
			}
			seen[chunk.DocumentID] = true

			if chunk.DocumentID == primary.document.ID {
				if len(item.ChunkIDs) != 2 || len(item.Matches) != 4 {
					t.Fatal("two fragments were not merged with complete provenance")
				}
				ids := map[uint64]bool{}
				for _, id := range item.ChunkIDs {
					ids[id] = true
				}
				if len(ids) != 2 || !ids[primary.chunk.ID] || !ids[second.ID] {
					t.Fatal("merged fragment identities differ")
				}
				scores := map[uint64]float64{}
				for _, match := range item.Matches {
					if match.Rank < 1 {
						t.Fatal("invalid real source rank")
					}
					scores[match.ChunkID] += 1.0 / float64(60+match.Rank)
				}
				wantScore := math.Max(scores[primary.chunk.ID], scores[second.ID])
				if math.Abs(item.RRFScore-wantScore) > 1e-15 {
					t.Fatal("article score must retain its best fragment RRF score")
				}
			}
		}
		vhReleased(t, s)
	})

	requireSubtest("filters", func(t *testing.T) {
		for _, name := range []string{
			"document_version_category", "category",
			"wrong_version", "wrong_category", "time_boundary",
		} {
			if !t.Run(name, func(t *testing.T) {
				opts := rglOptions(primary)
				at := s.at
				wantCount := 2
				switch name {
				case "category":
					opts.DocumentID = 0
					opts.VersionID = 0
				case "wrong_version":
					opts.VersionID = fixtures["revision"].version.ID
					wantCount = 0
				case "wrong_category":
					opts.CategoryID = fixtures["revision"].category.ID
					wantCount = 0
				case "time_boundary":
					timed := fixtures["timed"]
					opts = rglOptions(timed)
					at = *timed.version.ExpiresAt
					wantCount = 0
				}
				filtered := rglService(t, esClient, mvSearcher, repo, at)
				got, err := rglCall(t, dir, "filter-"+name, filtered, opts)
				vhMust(t, err)
				rglBoth(t, got, wantCount)
				if wantCount == 0 {
					rglEmpty(t, got, "no_candidates")
					if got.Stats.Hydration.Status != retrieval.HybridStageSkipped {
						t.Fatal("empty indexed result did not skip hydration")
					}
				} else if len(got.Items) != 1 ||
					got.Items[0].Rule.Chunk.DocumentID != primary.document.ID ||
					got.Items[0].Rule.Chunk.VersionID != primary.version.ID ||
					got.Items[0].Rule.Chunk.CategoryID != primary.category.ID {
					t.Fatal("real request filters were not preserved")
				}
			}) {
				t.Fatal("filter checkpoint failed")
			}
		}
	})

	requireSubtest("missing_aliases", func(t *testing.T) {
		missingES := rglESClient(t, "rag_missing_es_"+suffix)
		missingMVClient := rglMVClient(t, "rag_missing_mv_"+suffix)
		missingMV, err := milvus.NewSearcher(
			embedder, missingMVClient, 35*time.Second,
		)
		vhMust(t, err)

		for _, name := range []string{"es", "milvus", "both"} {
			if !t.Run(name, func(t *testing.T) {
				var es retrieval.CandidateSearcher = esClient
				var mv retrieval.CandidateSearcher = mvSearcher
				if name == "es" || name == "both" {
					es = missingES
				}
				if name == "milvus" || name == "both" {
					mv = missingMV
				}
				got, err := rglCall(
					t, dir, "missing-"+name,
					rglService(t, es, mv, repo, s.at), rglOptions(primary),
				)
				if name == "both" {
					if !errors.Is(err, retrieval.ErrHybridUnavailable) ||
						got.Items != nil ||
						got.Outcome != retrieval.HybridFailed ||
						got.Stats.Hydration.Status != retrieval.HybridStageNotRun {
						t.Fatal("double unavailable sources returned evidence")
					}
				} else {
					vhMust(t, err)
					if !got.Degraded || got.Outcome != retrieval.HybridDegraded ||
						len(got.Items) != 1 ||
						got.Items[0].Rule.ArticleContent != primary.article.Content {
						t.Fatal("single-source real degradation failed")
					}
				}
				for index, report := range got.Stats.Sources {
					failed := name == "both" ||
						(index == 0 && name == "es") ||
						(index == 1 && name == "milvus")
					if failed {
						if report.Status != retrieval.SourceFailed ||
							report.ErrorKind != retrieval.SourceErrorIndexNotReady {
							t.Fatalf("missing alias classification=%+v", report)
						}
					} else if report.Status != retrieval.SourceSucceeded ||
						report.RawCount != 2 {
						t.Fatalf("surviving source=%+v", report)
					}
				}
			}) {
				t.Fatal("missing-alias checkpoint failed")
			}
		}
	})

	requireSubtest("stale_current_state", func(t *testing.T) {
		for _, name := range []string{
			"revision", "replaced", "withdrawn", "expired", "category",
		} {
			if !t.Run(name, func(t *testing.T) {
				f := fixtures[name]
				switch name {
				case "revision":
					vhUpdate(t, s, "knowledge_documents", f.document.ID,
						"publication_revision", f.document.PublicationRevision+1)
				case "replaced":
					rglChangeCurrent(t, s, f, true)
				case "withdrawn":
					rglChangeCurrent(t, s, f, false)
				case "expired":
					vhUpdate(t, s, "rule_versions", f.version.ID,
						"expires_at", s.at)
				case "category":
					vhUpdate(t, s, "rule_versions", f.version.ID,
						"category_id", primary.category.ID)
				}

				// 故意不重建索引：两路仍命中旧快照。
				got, err := rglCall(
					t, dir, "stale-"+name, service, rglOptions(f),
				)
				vhMust(t, err)
				rglBoth(t, got, 1)
				rglEmpty(t, got, "current_filter")
				if got.Stats.FusedCount != 1 ||
					got.Stats.HydratedCount != 0 ||
					got.Stats.FilteredCount != 1 ||
					got.Stats.Hydration.Status != retrieval.HybridStageSuccess {
					t.Fatal("stale indexed rule bypassed authoritative hydration")
				}
				vhReleased(t, s)
			}) {
				t.Fatal("current-state checkpoint failed")
			}
		}
	})

	requireSubtest("bad_body_no_partial", func(t *testing.T) {
		f := fixtures["body"]
		vhUpdate(t, s, "rule_articles", f.article.ID, "content", " \t")
		got, err := rglCall(
			t, dir, "bad-body", service, retrieval.HybridOptions{TopK: 10},
		)
		if !errors.Is(err, retrieval.ErrHydrationFailed) ||
			got.Items != nil ||
			got.Outcome != retrieval.HybridFailed ||
			got.Stats.FinalCount != 0 ||
			got.Stats.Hydration.Status != retrieval.HybridStageFailed ||
			got.Stats.Rerank.Status != retrieval.HybridStageNotRun {
			t.Fatal("invalid authoritative body returned partial evidence")
		}
		rglBoth(t, got, 9)
		vhReleased(t, s)
	})

	requireSubtest("repeatable_read", func(t *testing.T) {
		// 在真实预算SELECT之后、真实正文SELECT之前提交发布变化。
		// 不替换SQL结果，不模拟事务。
		calls := vhDetailHook(t, s, func(_ *gorm.DB) error {
			result := s.writer.Model(&model.KnowledgeDocument{}).
				Where("id = ?", primary.document.ID).
				Updates(map[string]any{
					"current_version_id":   nil,
					"publication_revision": primary.document.PublicationRevision + 1,
				})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return fmt.Errorf("snapshot mutation affected %d rows",
					result.RowsAffected)
			}
			return nil
		})

		first, err := rglCall(
			t, dir, "snapshot-first", service, rglOptions(primary),
		)
		vhMust(t, err)
		rglBoth(t, first, 2)
		if *calls != 1 || len(first.Items) != 1 ||
			first.Items[0].Rule.ArticleContent != primary.article.Content {
			t.Fatal("hybrid hydration did not retain one read snapshot")
		}

		var current *uint64
		var revision uint64
		vhMust(t, s.writer.Raw(
			"SELECT current_version_id, publication_revision "+
				"FROM knowledge_documents WHERE id = ?",
			primary.document.ID,
		).Row().Scan(&current, &revision))
		if current != nil || revision != primary.document.PublicationRevision+1 {
			t.Fatal("independent publication mutation was not committed")
		}

		secondResult, err := rglCall(
			t, dir, "snapshot-second", service, rglOptions(primary),
		)
		vhMust(t, err)
		rglBoth(t, secondResult, 2)
		rglEmpty(t, secondResult, "current_filter")
		if *calls != 1 || secondResult.Stats.FilteredCount != 2 {
			t.Fatal("next hydration transaction did not observe the committed state")
		}
		vhReleased(t, s)
	})
}
