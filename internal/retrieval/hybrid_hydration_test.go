package retrieval_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"video-support-agent/internal/retrieval"
)

func ragHydrationFused(count int) []retrieval.FusedCandidate {
	items := make([]retrieval.FusedCandidate, count)
	for i := range items {
		id := uint64(i + 1)
		raw := float64(count - i)
		items[i] = retrieval.FusedCandidate{
			Candidate: ragFusionCandidate(id, raw),
			RRFScore:  1.0 / float64(61+i),
			Matches: []retrieval.SourceMatch{{
				Source:   retrieval.SourceES,
				ChunkID:  id,
				Rank:     i + 1,
				RawScore: raw,
			}},
		}
	}
	return items
}

func ragHydrationRow(candidate retrieval.Candidate) retrieval.HydratedChunk {
	chunk := candidate.Chunk
	if chunk.ExpiresAt != nil {
		value := *chunk.ExpiresAt
		chunk.ExpiresAt = &value
	}
	chunk.Title = "回表测试规则"
	chunk.SourceURL = "https://example.invalid/hydrated"
	chunk.SectionPath = "回表测试章节"
	chunk.ArticleNumber = "测试条款"
	chunk.ArticleOrder = 1
	chunk.ChunkIndex = uint32(chunk.ChunkID)
	chunk.Content = fmt.Sprintf("isolated chunk %d", chunk.ChunkID)
	chunk.Score = 0
	chunk.Highlights = map[string][]string{
		"content": {"stored highlight"},
	}
	return retrieval.HydratedChunk{
		Chunk:          chunk,
		ArticleContent: "isolated complete article",
	}
}

func ragHydrationRows(
	fused []retrieval.FusedCandidate,
) map[uint64]retrieval.HydratedChunk {
	rows := make(map[uint64]retrieval.HydratedChunk, len(fused))
	for _, item := range fused {
		rows[item.Candidate.Chunk.ChunkID] =
			ragHydrationRow(item.Candidate)
	}
	return rows
}

func ragHydrationResize(
	t *testing.T,
	row retrieval.HydratedChunk,
	total int,
) retrieval.HydratedChunk {
	t.Helper()
	c := row.Chunk
	fixed := len(c.Version) + len(c.Title) + len(c.SourceURL) +
		len(c.SectionPath) + len(c.ArticleNumber) + len(c.Content)
	if total <= fixed {
		t.Fatal("invalid test byte target")
	}
	row.ArticleContent = strings.Repeat("x", total-fixed)
	return row
}

func ragHydrationAssemble(
	t *testing.T,
	fused []retrieval.FusedCandidate,
	rows map[uint64]retrieval.HydratedChunk,
) []retrieval.HybridItem {
	t.Helper()
	items, err := retrieval.AssembleHybridItems(
		context.Background(), fused, rows,
		retrieval.HybridOptions{}, ragFusionAt(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if items == nil {
		t.Fatal("successful assembly returned nil")
	}
	return items
}

func TestRAGHydration_Prepare(t *testing.T) {
	fused := ragHydrationFused(3)
	fused[0].Matches = append(fused[0].Matches, retrieval.SourceMatch{
		Source: retrieval.SourceMilvus, ChunkID: 1, Rank: 2, RawScore: 0.8,
	})
	fused[0].RRFScore += 1.0 / 62
	before := ragFusionSnapshot(t, fused)

	got, err := retrieval.PrepareHybridHydration(
		context.Background(), fused,
		retrieval.HybridOptions{
			DocumentID: 11, VersionID: 21, CategoryID: 41, TopK: 1,
		}, ragFusionAt(),
	)
	if err != nil || len(got) != 3 {
		t.Fatalf("count=%d error=%v", len(got), err)
	}
	for i, candidate := range got {
		expected := fused[i].Candidate
		if candidate.Chunk.ChunkID != expected.Chunk.ChunkID ||
			candidate.Chunk.DocumentID != expected.Chunk.DocumentID ||
			candidate.Chunk.VersionID != expected.Chunk.VersionID ||
			candidate.Chunk.ArticleID != expected.Chunk.ArticleID ||
			candidate.Chunk.CategoryID != expected.Chunk.CategoryID ||
			candidate.Chunk.Version != expected.Chunk.Version ||
			candidate.PublicationRevision != expected.PublicationRevision {
			t.Fatal("hydration input identity changed")
		}
		ragFusionScore(t, candidate.Chunk.Score, fused[i].RRFScore)
		if candidate.Chunk.Title != "" || candidate.Chunk.Content != "" {
			t.Fatal("index body entered hydration input")
		}
	}
	if before != ragFusionSnapshot(t, fused) {
		t.Fatal("prepare modified original fusion result")
	}

	*got[0].Chunk.ExpiresAt = ragFusionAt().Add(3 * time.Hour)
	got[0].Chunk.Score = 999
	if before != ragFusionSnapshot(t, fused) {
		t.Fatal("prepared input aliases original data")
	}

	empty, err := retrieval.PrepareHybridHydration(
		context.Background(), []retrieval.FusedCandidate{},
		retrieval.HybridOptions{}, ragFusionAt(),
	)
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("empty input: result=%v error=%v", empty, err)
	}
	items := ragHydrationAssemble(t, []retrieval.FusedCandidate{},
		map[uint64]retrieval.HydratedChunk{})
	if len(items) != 0 {
		t.Fatal("empty fusion produced articles")
	}
}

func TestRAGHydration_FusedValidation(t *testing.T) {
	reject := func(
		t *testing.T,
		fused []retrieval.FusedCandidate,
		opts retrieval.HybridOptions,
		want error,
	) {
		t.Helper()
		prepared, err := retrieval.PrepareHybridHydration(
			context.Background(), fused, opts, ragFusionAt(),
		)
		if !errors.Is(err, want) || prepared != nil {
			t.Fatalf("prepare accepted invalid fusion: %v", err)
		}
		items, err := retrieval.AssembleHybridItems(
			context.Background(), fused,
			map[uint64]retrieval.HydratedChunk{}, opts, ragFusionAt(),
		)
		if !errors.Is(err, want) || items != nil {
			t.Fatalf("assemble accepted invalid fusion: %v", err)
		}
	}

	edits := []struct {
		name string
		edit func([]retrieval.FusedCandidate)
	}{
		{"no_matches", func(f []retrieval.FusedCandidate) {
			f[0].Matches = nil
		}},
		{"too_many_matches", func(f []retrieval.FusedCandidate) {
			m := f[0].Matches[0]
			f[0].Matches = []retrieval.SourceMatch{m, m, m}
		}},
		{"unknown_source", func(f []retrieval.FusedCandidate) {
			f[0].Matches[0].Source = "unknown"
		}},
		{"repeated_source", func(f []retrieval.FusedCandidate) {
			m := f[0].Matches[0]
			f[0].Matches = []retrieval.SourceMatch{m, m}
			f[0].RRFScore = 2.0 / 61
		}},
		{"source_order", func(f []retrieval.FusedCandidate) {
			es := f[0].Matches[0]
			f[0].Matches = []retrieval.SourceMatch{
				{Source: retrieval.SourceMilvus, ChunkID: 1, Rank: 1, RawScore: 0.8},
				es,
			}
			f[0].Candidate.Chunk.Score = 0.8
			f[0].RRFScore = 2.0 / 61
		}},
		{"zero_rank", func(f []retrieval.FusedCandidate) {
			f[0].Matches[0].Rank = 0
			f[0].RRFScore = 1.0 / 60
		}},
		{"rank_over_budget", func(f []retrieval.FusedCandidate) {
			f[2].Matches[0].Rank = 51
			f[2].RRFScore = 1.0 / 111
		}},
		{"match_id", func(f []retrieval.FusedCandidate) {
			f[0].Matches[0].ChunkID = 99
		}},
		{"raw_score_mismatch", func(f []retrieval.FusedCandidate) {
			f[0].Candidate.Chunk.Score++
		}},
		{"invalid_raw_score", func(f []retrieval.FusedCandidate) {
			f[0].Candidate.Chunk.Score = -1
			f[0].Matches[0].RawScore = -1
		}},
		{"milvus_range", func(f []retrieval.FusedCandidate) {
			f[0].Matches[0].Source = retrieval.SourceMilvus
			f[0].Matches[0].RawScore = 2
			f[0].Candidate.Chunk.Score = 2
		}},
		{"rrf_mismatch", func(f []retrieval.FusedCandidate) {
			f[0].RRFScore += 0.001
		}},
		{"rrf_nan", func(f []retrieval.FusedCandidate) {
			f[0].RRFScore = math.NaN()
		}},
		{"rrf_infinity", func(f []retrieval.FusedCandidate) {
			f[0].RRFScore = math.Inf(1)
		}},
		{"duplicate_id", func(f []retrieval.FusedCandidate) {
			f[1] = f[0]
		}},
		{"score_order", func(f []retrieval.FusedCandidate) {
			f[0], f[1] = f[1], f[0]
		}},
		{"tie_id_order", func(f []retrieval.FusedCandidate) {
			f[0], f[1] = f[1], f[0]
			for i := 0; i < 2; i++ {
				f[i].Matches[0].Rank = 1
				f[i].RRFScore = 1.0 / 61
			}
		}},
		{"invalid_identity", func(f []retrieval.FusedCandidate) {
			f[0].Candidate.Chunk.ArticleID = 0
		}},
	}
	for _, tt := range edits {
		t.Run(tt.name, func(t *testing.T) {
			fused := ragHydrationFused(3)
			tt.edit(fused)
			reject(t, fused, retrieval.HybridOptions{},
				retrieval.ErrInvalidCandidates)
		})
	}

	t.Run("nil", func(t *testing.T) {
		reject(t, nil, retrieval.HybridOptions{},
			retrieval.ErrInvalidCandidates)
	})
	t.Run("request_filter", func(t *testing.T) {
		reject(t, ragHydrationFused(3),
			retrieval.HybridOptions{VersionID: 22},
			retrieval.ErrInvalidCandidates)
	})
	t.Run("invalid_top_k", func(t *testing.T) {
		reject(t, ragHydrationFused(3),
			retrieval.HybridOptions{TopK: -1},
			retrieval.ErrInvalidQuery)
	})

	for _, tt := range []struct {
		count int
		topK  int
	}{{11, 1}, {251, 50}} {
		t.Run(fmt.Sprintf("count_%d", tt.count), func(t *testing.T) {
			fused := ragHydrationFused(tt.count)
			// Keep each item's rank valid to isolate the count limit.
			for i := range fused {
				fused[i].Matches[0].Rank = 1
				fused[i].RRFScore = 1.0 / 61
			}
			reject(t, fused, retrieval.HybridOptions{TopK: tt.topK},
				retrieval.ErrInvalidCandidates)
		})
	}
}

func TestRAGHydration_Assemble(t *testing.T) {
	fused := ragHydrationFused(3)
	fused[0].Candidate.Chunk.ArticleID = 501
	fused[1].Candidate.Chunk.ArticleID = 501
	rows := ragHydrationRows(fused)
	beforeFused := ragFusionSnapshot(t, fused)
	beforeRows := ragFusionSnapshot(t, rows)

	got, err := retrieval.AssembleHybridItems(
		context.Background(), fused, rows,
		retrieval.HybridOptions{TopK: 1}, ragFusionAt(),
	)
	if err != nil || len(got) != 2 {
		t.Fatalf("count=%d error=%v", len(got), err)
	}
	if got[0].Rule.Chunk.ChunkID != 1 ||
		got[1].Rule.Chunk.ChunkID != 3 ||
		!reflect.DeepEqual(got[0].ChunkIDs, []uint64{1, 2}) {
		t.Fatal("wrong article grouping or representative")
	}
	wantMatches := append(
		append([]retrieval.SourceMatch{}, fused[0].Matches...),
		fused[1].Matches...,
	)
	if !reflect.DeepEqual(got[0].Matches, wantMatches) {
		t.Fatal("article lost chunk provenance")
	}
	ragFusionScore(t, got[0].RRFScore, 1.0/61)
	ragFusionScore(t, got[0].Rule.Chunk.Score, 1.0/61)
	if got[0].Rule.Chunk.Title != rows[1].Chunk.Title ||
		got[0].Rule.Chunk.Content != rows[1].Chunk.Content ||
		got[0].Rule.ArticleContent != rows[1].ArticleContent ||
		len(got[0].Rule.Chunk.Highlights) != 0 {
		t.Fatal("wrong source of article text or metadata")
	}

	*got[0].Rule.Chunk.ExpiresAt = ragFusionAt().Add(3 * time.Hour)
	got[0].ChunkIDs[0] = 999
	got[0].Matches[0].RawScore = 999
	got[0].Rule.Chunk.Highlights["new"] = []string{"changed"}
	if beforeFused != ragFusionSnapshot(t, fused) ||
		beforeRows != ragFusionSnapshot(t, rows) {
		t.Fatal("assembled result modified or aliases inputs")
	}

	delete(rows, 1)
	missing := ragHydrationAssemble(t, fused, rows)
	if len(missing) != 2 || missing[0].Rule.Chunk.ChunkID != 2 {
		t.Fatal("missing best chunk did not select the next valid representative")
	}
	empty := ragHydrationAssemble(t, fused,
		map[uint64]retrieval.HydratedChunk{})
	if len(empty) != 0 {
		t.Fatal("missing IDs were not filtered")
	}

	for _, field := range []string{"document", "version"} {
		t.Run("separate_"+field, func(t *testing.T) {
			input := ragHydrationFused(2)
			input[0].Candidate.Chunk.ArticleID = 501
			input[1].Candidate.Chunk.ArticleID = 501
			if field == "document" {
				input[1].Candidate.Chunk.DocumentID++
			} else {
				input[1].Candidate.Chunk.VersionID++
			}
			items := ragHydrationAssemble(t, input, ragHydrationRows(input))
			if len(items) != 2 {
				t.Fatal("different document/version articles were merged")
			}
		})
	}
}

func TestRAGHydration_Rejects(t *testing.T) {
	reject := func(t *testing.T, fused []retrieval.FusedCandidate,
		rows map[uint64]retrieval.HydratedChunk) {
		t.Helper()
		got, err := retrieval.AssembleHybridItems(
			context.Background(), fused, rows,
			retrieval.HybridOptions{}, ragFusionAt(),
		)
		if !errors.Is(err, retrieval.ErrHydrationFailed) || got != nil {
			t.Fatalf("invalid hydration accepted: error=%v", err)
		}
	}

	for _, kind := range []string{"nil", "extra", "unknown", "key"} {
		t.Run("map_"+kind, func(t *testing.T) {
			fused := ragHydrationFused(2)
			rows := ragHydrationRows(fused)
			switch kind {
			case "nil":
				rows = nil
			case "extra":
				rows[99] = ragHydrationRow(ragFusionCandidate(99, 1))
			case "unknown":
				delete(rows, 1)
				rows[99] = ragHydrationRow(ragFusionCandidate(99, 1))
			case "key":
				row := rows[1]
				row.Chunk.ChunkID = 99
				rows[1] = row
			}
			reject(t, fused, rows)
		})
	}

	rowEdits := []struct {
		name string
		edit func(*retrieval.HydratedChunk)
	}{
		{"document", func(r *retrieval.HydratedChunk) { r.Chunk.DocumentID++ }},
		{"version_id", func(r *retrieval.HydratedChunk) { r.Chunk.VersionID++ }},
		{"article", func(r *retrieval.HydratedChunk) { r.Chunk.ArticleID++ }},
		{"category", func(r *retrieval.HydratedChunk) { r.Chunk.CategoryID++ }},
		{"version_text", func(r *retrieval.HydratedChunk) { r.Chunk.Version = "other" }},
		{"effective", func(r *retrieval.HydratedChunk) {
			r.Chunk.EffectiveAt = r.Chunk.EffectiveAt.Add(-time.Hour)
		}},
		{"expiration_nil", func(r *retrieval.HydratedChunk) { r.Chunk.ExpiresAt = nil }},
		{"expiration_value", func(r *retrieval.HydratedChunk) {
			value := ragFusionAt().Add(2 * time.Hour)
			r.Chunk.ExpiresAt = &value
		}},
		{"zero_article_order", func(r *retrieval.HydratedChunk) { r.Chunk.ArticleOrder = 0 }},
		{"blank_chunk", func(r *retrieval.HydratedChunk) { r.Chunk.Content = " \t" }},
		{"blank_article", func(r *retrieval.HydratedChunk) { r.ArticleContent = "\n " }},
		{"invalid_title_utf8", func(r *retrieval.HydratedChunk) {
			r.Chunk.Title = string([]byte{0xff})
		}},
		{"invalid_article_utf8", func(r *retrieval.HydratedChunk) {
			r.ArticleContent = string([]byte{0xff})
		}},
	}
	for _, tt := range rowEdits {
		t.Run("row_"+tt.name, func(t *testing.T) {
			fused := ragHydrationFused(2)
			rows := ragHydrationRows(fused)
			row := rows[2]
			tt.edit(&row)
			rows[2] = row
			reject(t, fused, rows)
		})
	}

	groupEdits := []struct {
		name string
		edit func(*retrieval.FusedCandidate, *retrieval.HydratedChunk)
	}{
		{"title", func(_ *retrieval.FusedCandidate, r *retrieval.HydratedChunk) {
			r.Chunk.Title = "different"
		}},
		{"url", func(_ *retrieval.FusedCandidate, r *retrieval.HydratedChunk) {
			r.Chunk.SourceURL = "https://example.invalid/other"
		}},
		{"section", func(_ *retrieval.FusedCandidate, r *retrieval.HydratedChunk) {
			r.Chunk.SectionPath = "different"
		}},
		{"number", func(_ *retrieval.FusedCandidate, r *retrieval.HydratedChunk) {
			r.Chunk.ArticleNumber = "different"
		}},
		{"order", func(_ *retrieval.FusedCandidate, r *retrieval.HydratedChunk) {
			r.Chunk.ArticleOrder++
		}},
		{"body", func(_ *retrieval.FusedCandidate, r *retrieval.HydratedChunk) {
			r.ArticleContent = "different complete article"
		}},
		{"revision", func(f *retrieval.FusedCandidate, _ *retrieval.HydratedChunk) {
			f.Candidate.PublicationRevision++
		}},
		{"category", func(f *retrieval.FusedCandidate, r *retrieval.HydratedChunk) {
			f.Candidate.Chunk.CategoryID++
			r.Chunk.CategoryID = f.Candidate.Chunk.CategoryID
		}},
		{"version_text", func(f *retrieval.FusedCandidate, r *retrieval.HydratedChunk) {
			f.Candidate.Chunk.Version = "different"
			r.Chunk.Version = "different"
		}},
		{"effective", func(f *retrieval.FusedCandidate, r *retrieval.HydratedChunk) {
			f.Candidate.Chunk.EffectiveAt = ragFusionAt().Add(-2 * time.Hour)
			r.Chunk.EffectiveAt = f.Candidate.Chunk.EffectiveAt
		}},
		{"expiration", func(f *retrieval.FusedCandidate, r *retrieval.HydratedChunk) {
			value := ragFusionAt().Add(2 * time.Hour)
			f.Candidate.Chunk.ExpiresAt = &value
			r.Chunk.ExpiresAt = &value
		}},
		{"expiration_nil", func(f *retrieval.FusedCandidate, r *retrieval.HydratedChunk) {
			f.Candidate.Chunk.ExpiresAt = nil
			r.Chunk.ExpiresAt = nil
		}},
	}
	for _, tt := range groupEdits {
		t.Run("group_"+tt.name, func(t *testing.T) {
			fused := ragHydrationFused(2)
			fused[1].Candidate.Chunk.ArticleID =
				fused[0].Candidate.Chunk.ArticleID
			rows := ragHydrationRows(fused)
			row := rows[2]
			tt.edit(&fused[1], &row)
			rows[2] = row
			reject(t, fused, rows)
		})
	}
}

func TestRAGHydration_Budgets(t *testing.T) {
	limit := retrieval.MaxHydrationRowBytes
	tests := []struct {
		name  string
		sizes []int
		bad   bool
	}{
		{"row_exact", []int{limit}, false},
		{"row_plus_one", []int{limit + 1}, true},
		{"total_exact",
			[]int{limit, limit, limit, limit, limit, limit, limit, limit},
			false},
		{"total_plus_one",
			[]int{limit, limit, limit, limit, limit, limit, limit,
				limit / 2, limit/2 + 1},
			true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fused := ragHydrationFused(len(tt.sizes))
			rows := ragHydrationRows(fused)
			for i, size := range tt.sizes {
				id := uint64(i + 1)
				rows[id] = ragHydrationResize(t, rows[id], size)
			}
			got, err := retrieval.AssembleHybridItems(
				context.Background(), fused, rows,
				retrieval.HybridOptions{}, ragFusionAt(),
			)
			if tt.bad {
				if !errors.Is(err, retrieval.ErrHydrationFailed) ||
					!errors.Is(err, retrieval.ErrHydrationLimit) ||
					got != nil {
					t.Fatalf("budget not enforced: error=%v", err)
				}
			} else if err != nil || len(got) != len(tt.sizes) {
				t.Fatalf("exact budget rejected: count=%d error=%v",
					len(got), err)
			}
		})
	}
}
