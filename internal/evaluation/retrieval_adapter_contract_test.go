package evaluation

import (
	"context"
	"strings"
	"testing"
	"time"

	"video-support-agent/internal/retrieval"
)

type contractRetriever func(context.Context, string, retrieval.HybridOptions) (retrieval.HybridResult, error)

func (f contractRetriever) Retrieve(ctx context.Context, q string, o retrieval.HybridOptions) (retrieval.HybridResult, error) {
	return f(ctx, q, o)
}

func adapterFixture(t *testing.T) (*CorpusBinding, Case, retrieval.HybridItem) {
	t.Helper()
	key := citation("a", "1")
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	corpus, err := NewCorpus(CorpusManifest{SchemaVersion: 1, CorpusID: "synthetic", Synthetic: true, Articles: []CorpusArticle{{Key: key, CategoryKey: "cat", Title: "合成规则", Content: "合成正文", Scope: "仅供隔离测试", EffectiveAt: at.Format(time.RFC3339)}}})
	if err != nil {
		t.Fatal(err)
	}
	ids := RuntimeArticleID{DocumentID: 1, VersionID: 2, ArticleID: 3}
	binding := &CorpusBinding{corpus: corpus, digest: strings.Repeat("b", 64), byID: map[RuntimeArticleID]CitationKey{ids: key}}
	binding.forward[3] = map[string]uint64{"cat": 4}
	c := contractCase("adapter")
	c.Suite = SuiteRetrieval
	c.ExpectedIntent = "retrieve"
	c.ExpectedHandoff = nil
	c.ExpectedDocuments = []string{"a"}
	item := retrieval.HybridItem{Rule: retrieval.HydratedChunk{Chunk: retrieval.RuleChunk{DocumentID: 1, VersionID: 2, ArticleID: 3, CategoryID: 4, Title: "合成规则", EffectiveAt: at}, ArticleContent: "合成正文"}}
	return binding, c, item
}

func TestRetrievalAdapterFailureAndCancellation(t *testing.T) {
	b, c, item := adapterFixture(t)
	for _, mode := range []string{"success", "unknown_identity", "timeout", "cancel_before"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "cancel_before" {
				cancel()
			}
			calls := 0
			retriever := contractRetriever(func(ctx context.Context, q string, opts retrieval.HybridOptions) (retrieval.HybridResult, error) {
				calls++
				if q != c.Question || opts.TopK != c.TopK {
					t.Fatal("request changed")
				}
				if mode == "timeout" {
					return retrieval.HybridResult{}, context.DeadlineExceeded
				}
				items := []retrieval.HybridItem{item}
				if mode == "unknown_identity" {
					unknown := item
					unknown.Rule.Chunk.ArticleID = 99
					items = append(items, unknown)
				}
				return retrieval.HybridResult{Outcome: retrieval.HybridSuccess, Items: items}, nil
			})
			rows, e := RunRetrieval(ctx, retriever, b, "adapter-run", []Case{c}, time.Second)
			if e != nil || len(rows) != 1 {
				t.Fatalf("record missing %v", e)
			}
			row := rows[0]
			switch mode {
			case "success":
				if row.Observation.Status != RunCompleted || len(row.Observation.Items) != 1 {
					t.Fatal("valid retrieval failed")
				}
			case "unknown_identity":
				if row.Observation.Status != RunFailed || row.ErrorCode != "corpus_mismatch" || len(row.Observation.Items) != 0 || len(row.RawIDs) != 2 || row.RawIDs[1].ArticleID != 99 {
					t.Fatal("unknown result dropped or raw rank lost")
				}
			case "timeout":
				if row.Observation.Status != RunTimedOut {
					t.Fatal("timeout not retained")
				}
			case "cancel_before":
				if calls != 0 || row.Observation.Status != RunCancelled {
					t.Fatal("cancelled request executed")
				}
			}
		})
	}
	var nilRetriever contractRetriever
	if _, e := RunRetrieval(context.Background(), nilRetriever, b, "adapter-run", []Case{c}, time.Second); e == nil {
		t.Fatal("typed nil retriever accepted")
	}
}
