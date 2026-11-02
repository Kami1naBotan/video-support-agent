package rag_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"video-support-agent/internal/rag"
	"video-support-agent/internal/retrieval"
)

var fixtureAt = time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)

// 明确虚构的内存测试资料；不写入知识库，也不访问这个URL。
func fixtureItem(n uint64) retrieval.HybridItem {
	return retrieval.HybridItem{
		Rule: retrieval.HydratedChunk{
			Chunk: retrieval.RuleChunk{
				ChunkID:       100 + n,
				DocumentID:    200 + n,
				VersionID:     300 + n,
				ArticleID:     400 + n,
				CategoryID:    1,
				ArticleOrder:  1,
				Version:       "test-v1",
				Title:         "虚构测试规则",
				SectionPath:   "测试/适用范围",
				ArticleNumber: "TEST-1",
				Content:       "仅供检索的测试片段",
				SourceURL:     "https://www.bilibili.com/__rag_fixture__",
				EffectiveAt:   fixtureAt.Add(-time.Hour),
			},
			ArticleContent: "虚构测试条款：满足条件A时可提交材料；条件B不适用。",
		},
		ChunkIDs: []uint64{100 + n},
	}
}

func fixtureResult(items ...retrieval.HybridItem) retrieval.HybridResult {
	if items == nil {
		items = []retrieval.HybridItem{}
	}
	result := retrieval.HybridResult{
		Items:       items,
		RetrievedAt: fixtureAt,
		Outcome:     retrieval.HybridSuccess,
		Stats: retrieval.HybridStats{
			FinalCount: len(items),
		},
	}
	if len(items) == 0 {
		result.Outcome = retrieval.HybridNoEvidence
		result.EmptyReason = "no_candidates"
	}
	return result
}

func requireEvidenceError(
	t *testing.T,
	set rag.EvidenceSet,
	err error,
	want error,
) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("error=%v, want %v", err, want)
	}
	if len(set.Rules()) != 0 || set.Stats() != (rag.EvidenceStats{}) {
		t.Fatalf("error returned partial evidence: %+v", set.Stats())
	}
}

func TestRAGEvidenceSnapshotAndCopy(t *testing.T) {
	item := fixtureItem(1)
	item.Rule.Chunk.DocumentID = ^uint64(0)
	item.Rule.Chunk.VersionID = 9007199254740993
	item.Rule.Chunk.ArticleID = 9007199254740995
	item.Rule.Chunk.ChunkID = 9007199254740997
	item.Rule.Chunk.ArticleNumber = ""
	item.ChunkIDs = []uint64{9007199254740997, 9007199254740999}

	expiry := fixtureAt.Add(time.Hour)
	item.Rule.Chunk.ExpiresAt = &expiry
	fullText := item.Rule.ArticleContent

	set, err := rag.BuildEvidence(
		context.Background(),
		[]retrieval.HybridItem{item},
		fixtureAt.In(time.FixedZone("test-zone", 8*60*60)),
	)
	if err != nil {
		t.Fatal(err)
	}
	rules := set.Rules()
	if len(rules) != 1 {
		t.Fatalf("rules=%d", len(rules))
	}

	c := rules[0].Citation
	if c.RefID != "R1" ||
		c.DocumentID != "18446744073709551615" ||
		c.VersionID != "9007199254740993" ||
		c.ArticleID != "9007199254740995" ||
		c.CategoryID != "1" ||
		c.ChunkID != "9007199254740997" {
		t.Fatalf("identity changed: %+v", c)
	}
	wantChunks := []string{"9007199254740997", "9007199254740999"}
	if !reflect.DeepEqual(c.ChunkIDs, wantChunks) {
		t.Fatalf("chunks=%v", c.ChunkIDs)
	}
	if c.ArticleNumber != "" ||
		c.Title != "虚构测试规则" ||
		c.Version != "test-v1" ||
		c.SectionPath != "测试/适用范围" ||
		c.SourceURL != "https://www.bilibili.com/__rag_fixture__" {
		t.Fatalf("snapshot metadata changed: %+v", c)
	}
	if rules[0].ArticleContent != fullText ||
		rules[0].ArticleContent == item.Rule.Chunk.Content {
		t.Fatal("complete article was replaced by a fragment")
	}
	if !c.RetrievedAt.Equal(fixtureAt) ||
		c.RetrievedAt.Location() != time.UTC ||
		!c.EffectiveAt.Equal(fixtureAt.Add(-time.Hour)) ||
		c.ExpiresAt == nil ||
		!c.ExpiresAt.Equal(fixtureAt.Add(time.Hour)) {
		t.Fatalf("unexpected snapshot times: %+v", c)
	}

	// 修改输入切片/指针，不应影响已经构造的证据。
	item.ChunkIDs[0] = 7
	expiry = fixtureAt.Add(10 * time.Hour)

	// 修改Rules()返回值，也不应影响下一次读取。
	rules[0].Citation.ChunkIDs[0] = "8"
	*rules[0].Citation.ExpiresAt = fixtureAt.Add(20 * time.Hour)
	rules[0].ArticleContent = "已修改"

	again := set.Rules()
	if !reflect.DeepEqual(again[0].Citation.ChunkIDs, wantChunks) ||
		!again[0].Citation.ExpiresAt.Equal(fixtureAt.Add(time.Hour)) ||
		again[0].ArticleContent != fullText {
		t.Fatal("evidence shares mutable state")
	}
}

func TestRAGEvidenceExclusions(t *testing.T) {
	items := []retrieval.HybridItem{
		fixtureItem(1), fixtureItem(2), fixtureItem(3),
		fixtureItem(4), fixtureItem(5),
	}
	items[1].Rule.Chunk.Title = " "
	items[2].Rule.Chunk.SourceURL = ""
	items[3].Rule.Chunk.EffectiveAt = fixtureAt.Add(time.Millisecond)

	set, err := rag.BuildEvidence(context.Background(), items, fixtureAt)
	if err != nil {
		t.Fatal(err)
	}
	want := rag.EvidenceStats{
		InputCount:           5,
		AcceptedCount:        2,
		InvalidMetadataCount: 1,
		InvalidSourceCount:   1,
		InactiveCount:        1,
	}
	if set.Stats() != want {
		t.Fatalf("stats=%+v, want %+v", set.Stats(), want)
	}
	rules := set.Rules()
	if len(rules) != 2 ||
		rules[0].Citation.RefID != "R1" ||
		rules[1].Citation.RefID != "R2" ||
		rules[1].Citation.ArticleID != "405" {
		t.Fatalf("accepted order/reference mapping changed: %+v", rules)
	}

	cases := []struct {
		name   string
		change func(*retrieval.HybridItem)
	}{
		{"blank_version", func(i *retrieval.HybridItem) {
			i.Rule.Chunk.Version = " "
		}},
		{"long_version", func(i *retrieval.HybridItem) {
			i.Rule.Chunk.Version = strings.Repeat("版", 65)
		}},
		{"long_title", func(i *retrieval.HybridItem) {
			i.Rule.Chunk.Title = strings.Repeat("题", 256)
		}},
		{"long_section", func(i *retrieval.HybridItem) {
			i.Rule.Chunk.SectionPath = strings.Repeat("章", 501)
		}},
		{"long_number", func(i *retrieval.HybridItem) {
			i.Rule.Chunk.ArticleNumber = strings.Repeat("号", 65)
		}},
		{"blank_article", func(i *retrieval.HybridItem) {
			i.Rule.ArticleContent = " \n "
		}},
		{"invalid_text_encoding", func(i *retrieval.HybridItem) {
			i.Rule.ArticleContent = string([]byte{0xff})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			item := fixtureItem(1)
			tc.change(&item)
			got, err := rag.BuildEvidence(
				context.Background(),
				[]retrieval.HybridItem{item},
				fixtureAt,
			)
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Rules()) != 0 ||
				got.Stats().InvalidMetadataCount != 1 {
				t.Fatalf("invalid metadata accepted: %+v", got.Stats())
			}
		})
	}
}

func TestRAGEvidenceIdentity(t *testing.T) {
	cases := []struct {
		name   string
		change func(*retrieval.HybridItem)
	}{
		{"document_zero", func(i *retrieval.HybridItem) {
			i.Rule.Chunk.DocumentID = 0
		}},
		{"version_zero", func(i *retrieval.HybridItem) {
			i.Rule.Chunk.VersionID = 0
		}},
		{"article_zero", func(i *retrieval.HybridItem) {
			i.Rule.Chunk.ArticleID = 0
		}},
		{"category_zero", func(i *retrieval.HybridItem) {
			i.Rule.Chunk.CategoryID = 0
		}},
		{"chunk_zero", func(i *retrieval.HybridItem) {
			i.Rule.Chunk.ChunkID = 0
		}},
		{"order_zero", func(i *retrieval.HybridItem) {
			i.Rule.Chunk.ArticleOrder = 0
		}},
		{"missing_provenance", func(i *retrieval.HybridItem) {
			i.ChunkIDs = nil
		}},
		{"zero_provenance", func(i *retrieval.HybridItem) {
			i.ChunkIDs = []uint64{101, 0}
		}},
		{"duplicate_provenance", func(i *retrieval.HybridItem) {
			i.ChunkIDs = []uint64{101, 101}
		}},
		{"missing_representative", func(i *retrieval.HybridItem) {
			i.ChunkIDs = []uint64{999}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			item := fixtureItem(1)
			tc.change(&item)
			set, err := rag.BuildEvidence(
				context.Background(),
				[]retrieval.HybridItem{item},
				fixtureAt,
			)
			requireEvidenceError(t, set, err, rag.ErrInvalidEvidence)
		})
	}

	t.Run("duplicate_article", func(t *testing.T) {
		item := fixtureItem(1)
		set, err := rag.BuildEvidence(
			context.Background(),
			[]retrieval.HybridItem{item, item},
			fixtureAt,
		)
		requireEvidenceError(t, set, err, rag.ErrInvalidEvidence)
	})

	t.Run("shared_fragment_between_articles", func(t *testing.T) {
		a, b := fixtureItem(1), fixtureItem(2)
		b.ChunkIDs = append(b.ChunkIDs, a.Rule.Chunk.ChunkID)
		set, err := rag.BuildEvidence(
			context.Background(),
			[]retrieval.HybridItem{a, b},
			fixtureAt,
		)
		requireEvidenceError(t, set, err, rag.ErrInvalidEvidence)
	})
}

func TestRAGEvidenceTimeWindow(t *testing.T) {
	before := fixtureAt.Add(-time.Millisecond)
	after := fixtureAt.Add(time.Millisecond)
	zero := time.Time{}
	cases := []struct {
		name      string
		effective time.Time
		expires   *time.Time
		accepted  int
	}{
		{"inclusive_start", fixtureAt, nil, 1},
		{"before_expiry", before, &after, 1},
		{"exclusive_expiry", before, &fixtureAt, 0},
		{"already_expired", before, &before, 0},
		{"future_start", after, nil, 0},
		{"missing_start", zero, nil, 0},
		{"invalid_expiry", before, &zero, 0},
		{"non_millisecond_start", before.Add(time.Nanosecond), nil, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			item := fixtureItem(1)
			item.Rule.Chunk.EffectiveAt = tc.effective
			item.Rule.Chunk.ExpiresAt = tc.expires
			set, err := rag.BuildEvidence(
				context.Background(),
				[]retrieval.HybridItem{item},
				fixtureAt,
			)
			if err != nil {
				t.Fatal(err)
			}
			if len(set.Rules()) != tc.accepted ||
				set.Stats().InactiveCount != 1-tc.accepted {
				t.Fatalf("unexpected time filtering: %+v", set.Stats())
			}
		})
	}
}

func TestRAGEvidenceLimitsAndCancellation(t *testing.T) {
	t.Run("nil_is_not_successful_empty", func(t *testing.T) {
		set, err := rag.BuildEvidence(context.Background(), nil, fixtureAt)
		requireEvidenceError(t, set, err, rag.ErrInvalidEvidence)
	})

	t.Run("explicit_empty", func(t *testing.T) {
		set, err := rag.BuildEvidence(
			context.Background(), []retrieval.HybridItem{}, fixtureAt,
		)
		if err != nil || len(set.Rules()) != 0 ||
			set.Stats() != (rag.EvidenceStats{}) {
			t.Fatalf("empty evidence: stats=%+v err=%v", set.Stats(), err)
		}
	})

	t.Run("too_many_articles", func(t *testing.T) {
		items := make([]retrieval.HybridItem, 6)
		for i := range items {
			items[i] = fixtureItem(uint64(i + 1))
		}
		set, err := rag.BuildEvidence(context.Background(), items, fixtureAt)
		requireEvidenceError(t, set, err, rag.ErrInvalidEvidence)
	})

	t.Run("fragment_budget", func(t *testing.T) {
		item := fixtureItem(1)
		item.ChunkIDs = make([]uint64, 250)
		for i := range item.ChunkIDs {
			item.ChunkIDs[i] = uint64(i + 1)
		}
		set, err := rag.BuildEvidence(
			context.Background(), []retrieval.HybridItem{item}, fixtureAt,
		)
		if err != nil || len(set.Rules()) != 1 {
			t.Fatalf("250 fragments rejected: %v", err)
		}
		item.ChunkIDs = append(item.ChunkIDs, 251)
		set, err = rag.BuildEvidence(
			context.Background(), []retrieval.HybridItem{item}, fixtureAt,
		)
		requireEvidenceError(t, set, err, rag.ErrEvidenceLimit)
	})

	t.Run("row_budget", func(t *testing.T) {
		item := fixtureItem(1)
		item.Rule.ArticleContent = strings.Repeat("x", 1<<20)
		set, err := rag.BuildEvidence(
			context.Background(), []retrieval.HybridItem{item}, fixtureAt,
		)
		requireEvidenceError(t, set, err, rag.ErrEvidenceLimit)
	})

	t.Run("invalid_retrieval_time", func(t *testing.T) {
		for _, at := range []time.Time{
			{}, fixtureAt.Add(time.Nanosecond),
		} {
			set, err := rag.BuildEvidence(
				context.Background(),
				[]retrieval.HybridItem{fixtureItem(1)},
				at,
			)
			requireEvidenceError(t, set, err, rag.ErrInvalidEvidence)
		}
	})

	t.Run("canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		set, err := rag.BuildEvidence(ctx, nil, time.Time{})
		requireEvidenceError(t, set, err, context.Canceled)
	})
}
