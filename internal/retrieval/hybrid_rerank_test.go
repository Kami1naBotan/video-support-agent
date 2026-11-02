package retrieval_test

import (
	"context"
	"errors"
	"math"
	"reflect"
	"testing"
	"time"

	"video-support-agent/internal/retrieval"
)

func ragRerankItems(count int) []retrieval.HybridItem {
	fused := ragHydrationFused(count)
	items := make([]retrieval.HybridItem, count)
	for i, item := range fused {
		rule := ragHydrationRow(item.Candidate)
		rule.Chunk.Score = item.RRFScore
		items[i] = retrieval.HybridItem{
			Rule:     rule,
			ChunkIDs: []uint64{rule.Chunk.ChunkID},
			RRFScore: item.RRFScore,
			Matches:  append([]retrieval.SourceMatch{}, item.Matches...),
		}
	}
	return items
}

func TestRAGRerank_Documents(t *testing.T) {
	items := ragRerankItems(3)
	items[0], items[2] = items[2], items[0]
	before := ragFusionSnapshot(t, items)

	documents, err := retrieval.BuildRerankDocuments(
		context.Background(), items,
	)
	if err != nil || len(documents) != 3 {
		t.Fatalf("count=%d error=%v", len(documents), err)
	}
	for i, document := range documents {
		item := items[i]
		if document.ChunkID != item.Rule.Chunk.ChunkID ||
			document.Title != item.Rule.Chunk.Title ||
			document.ArticleContent != item.Rule.ArticleContent ||
			document.RRFScore != item.RRFScore {
			t.Fatal("document mapping changed order or content")
		}
	}
	if documents[0].ChunkID != 3 || documents[2].ChunkID != 1 {
		t.Fatal("document construction sorted its input")
	}

	documents[0].Title = "changed"
	documents[0].ArticleContent = "changed"
	documents[0].ChunkID = 999
	documents[0].RRFScore = 999
	if before != ragFusionSnapshot(t, items) {
		t.Fatal("rerank documents modified original items")
	}

	empty, err := retrieval.BuildRerankDocuments(
		context.Background(), []retrieval.HybridItem{},
	)
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("empty documents: result=%v error=%v", empty, err)
	}
	reordered, err := retrieval.ApplyRerankOrder(
		context.Background(), []retrieval.HybridItem{}, []uint64{},
	)
	if err != nil || reordered == nil || len(reordered) != 0 {
		t.Fatalf("empty order: result=%v error=%v", reordered, err)
	}
}

func TestRAGRerank_Rejects(t *testing.T) {
	reject := func(t *testing.T, items []retrieval.HybridItem, budget bool) {
		t.Helper()
		documents, err := retrieval.BuildRerankDocuments(
			context.Background(), items,
		)
		if !errors.Is(err, retrieval.ErrInvalidRerank) || documents != nil {
			t.Fatalf("invalid documents accepted: error=%v", err)
		}
		if budget && !errors.Is(err, retrieval.ErrHydrationLimit) {
			t.Fatalf("missing budget error: %v", err)
		}

		order := make([]uint64, len(items))
		for i := range items {
			order[i] = items[i].Rule.Chunk.ChunkID
		}
		got, err := retrieval.ApplyRerankOrder(
			context.Background(), items, order,
		)
		if !errors.Is(err, retrieval.ErrInvalidRerank) || got != nil {
			t.Fatalf("invalid items entered order application: %v", err)
		}
	}

	t.Run("nil", func(t *testing.T) {
		reject(t, nil, false)
	})
	t.Run("too_many", func(t *testing.T) {
		reject(t, ragRerankItems(251), false)
	})

	for index, name := range []string{
		"chunk", "document", "version", "article", "category",
	} {
		t.Run("zero_"+name, func(t *testing.T) {
			items := ragRerankItems(2)
			c := &items[0].Rule.Chunk
			fields := []*uint64{
				&c.ChunkID, &c.DocumentID, &c.VersionID,
				&c.ArticleID, &c.CategoryID,
			}
			*fields[index] = 0
			reject(t, items, false)
		})
	}

	edits := []struct {
		name string
		edit func([]retrieval.HybridItem)
	}{
		{"duplicate_representative", func(items []retrieval.HybridItem) {
			items[1].Rule.Chunk.ChunkID = items[0].Rule.Chunk.ChunkID
		}},
		{"duplicate_article", func(items []retrieval.HybridItem) {
			items[1].Rule.Chunk.ArticleID = items[0].Rule.Chunk.ArticleID
		}},
		{"chunk_score_mismatch", func(items []retrieval.HybridItem) {
			items[0].Rule.Chunk.Score = 0
		}},
		{"blank_chunk", func(items []retrieval.HybridItem) {
			items[0].Rule.Chunk.Content = " \t"
		}},
		{"blank_article", func(items []retrieval.HybridItem) {
			items[0].Rule.ArticleContent = "\n "
		}},
		{"invalid_utf8", func(items []retrieval.HybridItem) {
			items[0].Rule.Chunk.Title = string([]byte{0xff})
		}},
		{"zero_order", func(items []retrieval.HybridItem) {
			items[0].Rule.Chunk.ArticleOrder = 0
		}},
	}
	for _, tt := range edits {
		t.Run(tt.name, func(t *testing.T) {
			items := ragRerankItems(2)
			tt.edit(items)
			reject(t, items, false)
		})
	}

	for _, tt := range []struct {
		name  string
		score float64
	}{
		{"zero_score", 0},
		{"negative_score", -1},
		{"large_score", 0.1},
		{"nan_score", math.NaN()},
		{"infinite_score", math.Inf(1)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			items := ragRerankItems(2)
			items[0].RRFScore = tt.score
			items[0].Rule.Chunk.Score = tt.score
			reject(t, items, false)
		})
	}

	t.Run("budget_boundaries", func(t *testing.T) {
		limit := retrieval.MaxHydrationRowBytes
		items := ragRerankItems(8)
		for i := range items {
			items[i].Rule = ragHydrationResize(t, items[i].Rule, limit)
		}
		documents, err := retrieval.BuildRerankDocuments(
			context.Background(), items,
		)
		if err != nil || len(documents) != 8 {
			t.Fatalf("exact total budget rejected: %v", err)
		}

		tooLargeRow := ragRerankItems(1)
		tooLargeRow[0].Rule = ragHydrationResize(
			t, tooLargeRow[0].Rule, limit+1,
		)
		reject(t, tooLargeRow, true)

		total := ragRerankItems(9)
		for i := range total {
			size := limit
			if i == 7 {
				size = limit / 2
			} else if i == 8 {
				size = limit/2 + 1
			}
			total[i].Rule = ragHydrationResize(t, total[i].Rule, size)
		}
		reject(t, total, true)
	})
}

func TestRAGRerank_Order(t *testing.T) {
	items := ragRerankItems(3)
	before := ragFusionSnapshot(t, items)
	order := []uint64{3, 1, 2}

	got, err := retrieval.ApplyRerankOrder(
		context.Background(), items, order,
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []retrieval.HybridItem{items[2], items[0], items[1]}
	if !reflect.DeepEqual(got, want) {
		t.Fatal("reordering changed article data, scores or provenance")
	}
	if !reflect.DeepEqual(order, []uint64{3, 1, 2}) {
		t.Fatal("input order was modified")
	}

	*got[0].Rule.Chunk.ExpiresAt = ragFusionAt().Add(3 * time.Hour)
	got[0].Rule.Chunk.Highlights["content"][0] = "changed"
	got[0].ChunkIDs[0] = 999
	got[0].Matches[0].RawScore = 999
	if before != ragFusionSnapshot(t, items) {
		t.Fatal("reordered result aliases original nested data")
	}

	for _, tt := range []struct {
		name  string
		order []uint64
	}{
		{"nil", nil},
		{"missing", []uint64{1, 2}},
		{"extra", []uint64{1, 2, 3, 4}},
		{"unknown", []uint64{1, 2, 99}},
		{"duplicate", []uint64{1, 1, 3}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			output, err := retrieval.ApplyRerankOrder(
				context.Background(), items, tt.order,
			)
			if !errors.Is(err, retrieval.ErrInvalidRerank) || output != nil {
				t.Fatalf("invalid order accepted: error=%v", err)
			}
		})
	}
}

func TestRAGContracts_Cancellation(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, stop := context.WithDeadline(
		context.Background(), time.Now().Add(-time.Second),
	)
	defer stop()

	fused := ragHydrationFused(1)
	rows := ragHydrationRows(fused)
	items := ragRerankItems(1)

	for _, tt := range []struct {
		name string
		ctx  context.Context
		want error
	}{
		{"canceled", canceled, context.Canceled},
		{"expired", expired, context.DeadlineExceeded},
	} {
		t.Run(tt.name, func(t *testing.T) {
			prepared, err := retrieval.PrepareHybridHydration(
				tt.ctx, fused, retrieval.HybridOptions{}, ragFusionAt(),
			)
			if !errors.Is(err, tt.want) || prepared != nil {
				t.Fatalf("prepare cancellation: %v", err)
			}
			assembled, err := retrieval.AssembleHybridItems(
				tt.ctx, fused, rows,
				retrieval.HybridOptions{}, ragFusionAt(),
			)
			if !errors.Is(err, tt.want) || assembled != nil {
				t.Fatalf("assembly cancellation: %v", err)
			}
			documents, err := retrieval.BuildRerankDocuments(tt.ctx, items)
			if !errors.Is(err, tt.want) || documents != nil {
				t.Fatalf("document cancellation: %v", err)
			}
			reordered, err := retrieval.ApplyRerankOrder(
				tt.ctx, items, []uint64{1},
			)
			if !errors.Is(err, tt.want) || reordered != nil {
				t.Fatalf("order cancellation: %v", err)
			}
		})
	}
}
