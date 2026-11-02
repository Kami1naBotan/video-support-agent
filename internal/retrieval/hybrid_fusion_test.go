package retrieval_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"video-support-agent/internal/retrieval"
)

func ragFusionAt() time.Time {
	return time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
}

func ragFusionCandidate(id uint64, score float64) retrieval.Candidate {
	expires := ragFusionAt().Add(time.Hour)
	return retrieval.Candidate{
		PublicationRevision: 7,
		Chunk: retrieval.RuleChunk{
			ChunkID:       id,
			DocumentID:    11,
			VersionID:     21,
			ArticleID:     100 + id,
			CategoryID:    41,
			ChunkIndex:    3,
			ArticleOrder:  1,
			Version:       "test-v1",
			Title:         "隔离测试规则",
			SectionPath:   "测试章节",
			ArticleNumber: "测试条款",
			Content:       "仅供内存测试的虚构片段",
			SourceURL:     "https://example.invalid/test",
			EffectiveAt:   ragFusionAt().Add(-time.Hour),
			ExpiresAt:     &expires,
			Score:         score,
			Highlights: map[string][]string{
				"content": {"测试高亮"},
			},
		},
	}
}

func ragFusionSnapshot(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func ragFusionRun(
	t *testing.T,
	lists []retrieval.RankedCandidates,
	opts retrieval.HybridOptions,
) retrieval.FusionResult {
	t.Helper()
	got, err := retrieval.FuseCandidates(
		context.Background(), lists, opts, ragFusionAt(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if got.Candidates == nil {
		t.Fatal("successful fusion returned a nil slice")
	}
	return got
}

func ragFusionScore(t *testing.T, got, want float64) {
	t.Helper()
	if math.IsNaN(got) || math.IsInf(got, 0) ||
		math.Abs(got-want) > 1e-15 {
		t.Fatalf("score=%v, want %v", got, want)
	}
}

func ragFusionBadFreeze(
	t *testing.T,
	list retrieval.RankedCandidates,
	opts retrieval.HybridOptions,
	at time.Time,
	want error,
) {
	t.Helper()
	got, err := retrieval.FreezeRankedCandidates(
		context.Background(), list, opts, at,
	)
	if !errors.Is(err, want) {
		t.Fatalf("error=%v, want %v", err, want)
	}
	if !reflect.DeepEqual(got, retrieval.RankedCandidates{}) {
		t.Fatalf("error returned partial result: %+v", got)
	}
}

func TestRAGFusion_Normalize(t *testing.T) {
	tests := []struct {
		name  string
		query string
		topK  int
		want  string
		limit int
		bad   bool
	}{
		{"trim_default", " \t测试问题\n", 0, "测试问题", 10, false},
		{"explicit_max", "测试", 50, "测试", 50, false},
		{"rune_boundary", strings.Repeat("问", 200), 1,
			strings.Repeat("问", 200), 1, false},
		{"byte_boundary",
			strings.Repeat(" ", retrieval.MaxHybridQueryBytes-1) + "x",
			0, "x", 10, false},
		{"empty", "", 0, "", 0, true},
		{"whitespace", " \t\n", 0, "", 0, true},
		{"invalid_utf8", string([]byte{0xff}), 0, "", 0, true},
		{"too_many_runes", strings.Repeat("问", 201), 0, "", 0, true},
		{"too_many_bytes",
			strings.Repeat(" ", retrieval.MaxHybridQueryBytes) + "x",
			0, "", 0, true},
		{"negative_top_k", "测试", -1, "", 0, true},
		{"large_top_k", "测试", 51, "", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := retrieval.HybridOptions{
				DocumentID: 11,
				VersionID:  21,
				CategoryID: 41,
				TopK:       tt.topK,
			}
			query, opts, err := retrieval.NormalizeHybridSearch(
				tt.query, input,
			)

			if tt.bad {
				if !errors.Is(err, retrieval.ErrInvalidQuery) {
					t.Fatalf("error=%v", err)
				}
				if query != "" || opts != (retrieval.HybridOptions{}) {
					t.Fatal("invalid input returned a partial result")
				}
				return
			}

			if err != nil {
				t.Fatal(err)
			}
			want := input
			want.TopK = tt.limit
			if query != tt.want || opts != want {
				t.Fatalf("query=%q opts=%+v; want %q %+v",
					query, opts, tt.want, want)
			}
		})
	}
}

func TestRAGFusion_FreezeRejects(t *testing.T) {
	checkCandidate := func(t *testing.T, candidate retrieval.Candidate) {
		t.Helper()
		ragFusionBadFreeze(t, retrieval.RankedCandidates{
			Source:     retrieval.SourceES,
			Candidates: []retrieval.Candidate{candidate},
		}, retrieval.HybridOptions{}, ragFusionAt(),
			retrieval.ErrInvalidCandidates)
	}

	for index, name := range []string{
		"chunk_id", "document_id", "version_id", "article_id", "category_id",
	} {
		t.Run("zero_"+name, func(t *testing.T) {
			candidate := ragFusionCandidate(1, 1)
			fields := []*uint64{
				&candidate.Chunk.ChunkID,
				&candidate.Chunk.DocumentID,
				&candidate.Chunk.VersionID,
				&candidate.Chunk.ArticleID,
				&candidate.Chunk.CategoryID,
			}
			*fields[index] = 0
			checkCandidate(t, candidate)
		})
	}

	edits := []struct {
		name string
		edit func(*retrieval.Candidate)
	}{
		{"nan_score", func(c *retrieval.Candidate) {
			c.Chunk.Score = math.NaN()
		}},
		{"infinite_score", func(c *retrieval.Candidate) {
			c.Chunk.Score = math.Inf(1)
		}},
		{"negative_es_score", func(c *retrieval.Candidate) {
			c.Chunk.Score = -0.01
		}},
		{"blank_version", func(c *retrieval.Candidate) {
			c.Chunk.Version = " \t"
		}},
		{"invalid_version_utf8", func(c *retrieval.Candidate) {
			c.Chunk.Version = string([]byte{0xff})
		}},
		{"long_version", func(c *retrieval.Candidate) {
			c.Chunk.Version = strings.Repeat("v", 257)
		}},
		{"future_effective", func(c *retrieval.Candidate) {
			c.Chunk.EffectiveAt = ragFusionAt().Add(time.Millisecond)
		}},
		{"zero_effective", func(c *retrieval.Candidate) {
			c.Chunk.EffectiveAt = time.Time{}
		}},
		{"expires_at_query_time", func(c *retrieval.Candidate) {
			value := ragFusionAt()
			c.Chunk.ExpiresAt = &value
		}},
		{"expires_before_effective", func(c *retrieval.Candidate) {
			value := ragFusionAt().Add(-2 * time.Hour)
			c.Chunk.ExpiresAt = &value
		}},
		{"expiration_year", func(c *retrieval.Candidate) {
			value := time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
			c.Chunk.ExpiresAt = &value
		}},
	}
	for _, tt := range edits {
		t.Run(tt.name, func(t *testing.T) {
			candidate := ragFusionCandidate(1, 1)
			tt.edit(&candidate)
			checkCandidate(t, candidate)
		})
	}

	for _, score := range []float64{-1.01, 1.01} {
		t.Run(fmt.Sprintf("milvus_range_%v", score), func(t *testing.T) {
			ragFusionBadFreeze(t, retrieval.RankedCandidates{
				Source: retrieval.SourceMilvus,
				Candidates: []retrieval.Candidate{
					ragFusionCandidate(1, score),
				},
			}, retrieval.HybridOptions{}, ragFusionAt(),
				retrieval.ErrInvalidCandidates)
		})
	}

	valid := retrieval.RankedCandidates{
		Source: retrieval.SourceES,
		Candidates: []retrieval.Candidate{
			ragFusionCandidate(1, 1),
		},
	}

	for _, tt := range []struct {
		name string
		opts retrieval.HybridOptions
	}{
		{"document_filter", retrieval.HybridOptions{DocumentID: 12}},
		{"version_filter", retrieval.HybridOptions{VersionID: 22}},
		{"category_filter", retrieval.HybridOptions{CategoryID: 42}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ragFusionBadFreeze(t, valid, tt.opts, ragFusionAt(),
				retrieval.ErrInvalidCandidates)
		})
	}

	t.Run("unknown_source", func(t *testing.T) {
		list := valid
		list.Source = "unknown"
		ragFusionBadFreeze(t, list, retrieval.HybridOptions{},
			ragFusionAt(), retrieval.ErrInvalidCandidates)
	})
	t.Run("nil_success_slice", func(t *testing.T) {
		ragFusionBadFreeze(t, retrieval.RankedCandidates{
			Source: retrieval.SourceES,
		}, retrieval.HybridOptions{}, ragFusionAt(),
			retrieval.ErrInvalidCandidates)
	})
	t.Run("invalid_top_k", func(t *testing.T) {
		for _, topK := range []int{-1, 51} {
			ragFusionBadFreeze(t, valid,
				retrieval.HybridOptions{TopK: topK},
				ragFusionAt(), retrieval.ErrInvalidQuery)
		}
	})
	t.Run("invalid_time", func(t *testing.T) {
		for _, at := range []time.Time{
			{},
			time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC),
		} {
			ragFusionBadFreeze(t, valid, retrieval.HybridOptions{},
				at, retrieval.ErrInvalidCandidates)
		}
	})

	for _, count := range []int{6, 251} {
		t.Run(fmt.Sprintf("budget_%d", count), func(t *testing.T) {
			topK := 1
			if count == 251 {
				topK = 50
			}
			list := retrieval.RankedCandidates{
				Source:     retrieval.SourceES,
				Candidates: make([]retrieval.Candidate, count),
			}
			for i := range list.Candidates {
				list.Candidates[i] = ragFusionCandidate(uint64(i+1), 1)
			}
			ragFusionBadFreeze(t, list,
				retrieval.HybridOptions{TopK: topK},
				ragFusionAt(), retrieval.ErrInvalidCandidates)
		})
	}

	for _, tt := range []struct {
		name   string
		source retrieval.RetrievalSource
		first  float64
		second float64
	}{
		{"es_order", retrieval.SourceES, 1, 2},
		{"milvus_order", retrieval.SourceMilvus, 0.5, 0.500002},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ragFusionBadFreeze(t, retrieval.RankedCandidates{
				Source: tt.source,
				Candidates: []retrieval.Candidate{
					ragFusionCandidate(1, tt.first),
					ragFusionCandidate(2, tt.second),
				},
			}, retrieval.HybridOptions{}, ragFusionAt(),
				retrieval.ErrInvalidCandidates)
		})
	}
}

func TestRAGFusion_FreezeBoundariesAndCopy(t *testing.T) {
	t.Run("canonical_copy", func(t *testing.T) {
		zone := time.FixedZone("test-zone", 8*60*60)
		at := ragFusionAt().In(zone).Add(999 * time.Microsecond)
		candidate := ragFusionCandidate(7, 12)
		candidate.Chunk.EffectiveAt = at
		expires := at.Add(time.Hour)
		candidate.Chunk.ExpiresAt = &expires

		input := retrieval.RankedCandidates{
			Source:     retrieval.SourceES,
			Candidates: []retrieval.Candidate{candidate},
		}
		before := ragFusionSnapshot(t, input)

		got, err := retrieval.FreezeRankedCandidates(
			context.Background(), input,
			retrieval.HybridOptions{
				DocumentID: 11, VersionID: 21, CategoryID: 41, TopK: 1,
			}, at,
		)
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Candidates) != 1 || got.Source != retrieval.SourceES {
			t.Fatalf("unexpected result: %+v", got)
		}
		chunk := got.Candidates[0].Chunk
		if !chunk.EffectiveAt.Equal(ragFusionAt()) ||
			chunk.EffectiveAt.Location() != time.UTC ||
			chunk.ExpiresAt == nil ||
			!chunk.ExpiresAt.Equal(ragFusionAt().Add(time.Hour)) {
			t.Fatalf("time was not canonicalized: %+v", chunk)
		}
		if chunk.Title != "" || chunk.Content != "" ||
			chunk.SourceURL != "" || chunk.Highlights != nil ||
			chunk.SectionPath != "" || chunk.ArticleNumber != "" ||
			chunk.ArticleOrder != 0 || chunk.ChunkIndex != 0 {
			t.Fatal("index payload leaked into frozen metadata")
		}
		if chunk.Score != 12 ||
			got.Candidates[0].PublicationRevision != 7 {
			t.Fatal("raw score or publication revision changed")
		}
		if before != ragFusionSnapshot(t, input) {
			t.Fatal("freezing modified input")
		}

		*got.Candidates[0].Chunk.ExpiresAt = ragFusionAt().Add(3 * time.Hour)
		got.Candidates[0].Chunk.Score = 999
		if before != ragFusionSnapshot(t, input) {
			t.Fatal("frozen result aliases input")
		}
	})

	for _, tt := range []struct {
		source retrieval.RetrievalSource
		score  float64
	}{
		{retrieval.SourceES, 0},
		{retrieval.SourceES, 1000000},
		{retrieval.SourceMilvus, -1},
		{retrieval.SourceMilvus, 1},
	} {
		t.Run(fmt.Sprintf("%s_score_%v", tt.source, tt.score),
			func(t *testing.T) {
				candidate := ragFusionCandidate(1, tt.score)
				candidate.PublicationRevision = 0
				candidate.Chunk.Version = strings.Repeat("v", 256)
				candidate.Chunk.EffectiveAt = ragFusionAt()
				candidate.Chunk.ExpiresAt = nil

				got, err := retrieval.FreezeRankedCandidates(
					context.Background(),
					retrieval.RankedCandidates{
						Source:     tt.source,
						Candidates: []retrieval.Candidate{candidate},
					},
					retrieval.HybridOptions{}, ragFusionAt(),
				)
				if err != nil || len(got.Candidates) != 1 {
					t.Fatalf("boundary rejected: %+v %v", got, err)
				}
				if got.Candidates[0].Chunk.Score != tt.score ||
					got.Candidates[0].PublicationRevision != 0 {
					t.Fatal("valid boundary values changed")
				}
			})
	}

	t.Run("empty_and_milvus_tolerance", func(t *testing.T) {
		empty, err := retrieval.FreezeRankedCandidates(
			context.Background(),
			retrieval.RankedCandidates{
				Source:     retrieval.SourceMilvus,
				Candidates: []retrieval.Candidate{},
			},
			retrieval.HybridOptions{}, ragFusionAt(),
		)
		if err != nil || empty.Candidates == nil ||
			len(empty.Candidates) != 0 {
			t.Fatalf("successful empty result lost: %+v %v", empty, err)
		}

		list := retrieval.RankedCandidates{
			Source: retrieval.SourceMilvus,
			Candidates: []retrieval.Candidate{
				ragFusionCandidate(1, 0.5),
				ragFusionCandidate(2, 0.5000005),
			},
		}
		got, err := retrieval.FreezeRankedCandidates(
			context.Background(), list,
			retrieval.HybridOptions{}, ragFusionAt(),
		)
		if err != nil {
			t.Fatal(err)
		}
		if got.Candidates[0].Chunk.ChunkID != 1 ||
			got.Candidates[1].Chunk.Score != 0.5000005 {
			t.Fatal("tolerated scores were reordered or rewritten")
		}
	})
}

func TestRAGFusion_RanksAndDuplicates(t *testing.T) {
	lists := []retrieval.RankedCandidates{
		{
			Source: retrieval.SourceES,
			Candidates: []retrieval.Candidate{
				ragFusionCandidate(10, 9),
				ragFusionCandidate(10, 8),
				ragFusionCandidate(20, 7),
				ragFusionCandidate(30, 6),
			},
		},
		{
			Source: retrieval.SourceMilvus,
			Candidates: []retrieval.Candidate{
				ragFusionCandidate(20, 0.9),
				ragFusionCandidate(10, 0.8),
				ragFusionCandidate(40, 0.7),
			},
		},
	}
	before := ragFusionSnapshot(t, lists)
	got := ragFusionRun(t, lists, retrieval.HybridOptions{TopK: 2})

	if got.RawCount != 7 || got.DuplicateCount != 3 ||
		got.ConflictCount != 0 || got.UniqueCount != 4 ||
		got.ClippedCount != 0 || len(got.Candidates) != 4 {
		t.Fatalf("wrong fusion counts: %+v", got)
	}

	wantIDs := []uint64{10, 20, 40, 30}
	wantScores := []float64{
		1.0/61 + 1.0/62,
		1.0/63 + 1.0/61,
		1.0 / 63,
		1.0 / 64,
	}
	for i, wantID := range wantIDs {
		if got.Candidates[i].Candidate.Chunk.ChunkID != wantID {
			t.Fatalf("position %d: want ID %d", i, wantID)
		}
		ragFusionScore(t, got.Candidates[i].RRFScore, wantScores[i])
	}

	wantMatches := []retrieval.SourceMatch{
		{Source: retrieval.SourceES, ChunkID: 10, Rank: 1, RawScore: 9},
		{Source: retrieval.SourceMilvus, ChunkID: 10, Rank: 2, RawScore: 0.8},
	}
	if !reflect.DeepEqual(got.Candidates[0].Matches, wantMatches) {
		t.Fatalf("wrong provenance: %+v", got.Candidates[0].Matches)
	}
	if got.Candidates[1].Matches[0].Rank != 3 ||
		got.Candidates[1].Candidate.Chunk.Score != 7 {
		t.Fatal("duplicate compressed ranks or raw score was replaced")
	}

	reversed := ragFusionRun(t,
		[]retrieval.RankedCandidates{lists[1], lists[0]},
		retrieval.HybridOptions{TopK: 2})
	if !reflect.DeepEqual(got, reversed) {
		t.Fatal("source arrival order changed fusion")
	}
	if before != ragFusionSnapshot(t, lists) {
		t.Fatal("fusion modified input")
	}

	*got.Candidates[0].Candidate.Chunk.ExpiresAt =
		ragFusionAt().Add(4 * time.Hour)
	got.Candidates[0].Matches[0].RawScore = 999
	if before != ragFusionSnapshot(t, lists) {
		t.Fatal("fusion result aliases input")
	}
}

func TestRAGFusion_Conflicts(t *testing.T) {
	edits := []struct {
		name string
		edit func(*retrieval.Candidate)
	}{
		{"document", func(c *retrieval.Candidate) { c.Chunk.DocumentID++ }},
		{"version_id", func(c *retrieval.Candidate) { c.Chunk.VersionID++ }},
		{"article", func(c *retrieval.Candidate) { c.Chunk.ArticleID++ }},
		{"category", func(c *retrieval.Candidate) { c.Chunk.CategoryID++ }},
		{"version_text", func(c *retrieval.Candidate) { c.Chunk.Version = "test-v2" }},
		{"revision", func(c *retrieval.Candidate) { c.PublicationRevision++ }},
		{"effective", func(c *retrieval.Candidate) {
			c.Chunk.EffectiveAt = ragFusionAt().Add(-2 * time.Hour)
		}},
		{"expiration_nil", func(c *retrieval.Candidate) {
			c.Chunk.ExpiresAt = nil
		}},
		{"expiration_value", func(c *retrieval.Candidate) {
			value := ragFusionAt().Add(2 * time.Hour)
			c.Chunk.ExpiresAt = &value
		}},
	}

	for _, tt := range edits {
		t.Run(tt.name, func(t *testing.T) {
			conflict := ragFusionCandidate(1, 8)
			tt.edit(&conflict)
			got := ragFusionRun(t, []retrieval.RankedCandidates{
				{
					Source: retrieval.SourceES,
					Candidates: []retrieval.Candidate{
						ragFusionCandidate(1, 9),
						conflict,
						ragFusionCandidate(1, 7),
						ragFusionCandidate(2, 6),
					},
				},
				{
					Source: retrieval.SourceMilvus,
					Candidates: []retrieval.Candidate{
						ragFusionCandidate(1, 0.9),
					},
				},
			}, retrieval.HybridOptions{})

			if got.RawCount != 5 || got.DuplicateCount != 3 ||
				got.ConflictCount != 1 || got.UniqueCount != 1 ||
				len(got.Candidates) != 1 ||
				got.Candidates[0].Candidate.Chunk.ChunkID != 2 {
				t.Fatalf("conflict was retained or revived: %+v", got)
			}
			ragFusionScore(t, got.Candidates[0].RRFScore, 1.0/64)
		})
	}

	t.Run("all_conflicting", func(t *testing.T) {
		conflict := ragFusionCandidate(1, 0.9)
		conflict.PublicationRevision++
		got := ragFusionRun(t, []retrieval.RankedCandidates{
			{Source: retrieval.SourceES,
				Candidates: []retrieval.Candidate{ragFusionCandidate(1, 9)}},
			{Source: retrieval.SourceMilvus,
				Candidates: []retrieval.Candidate{conflict}},
		}, retrieval.HybridOptions{})
		if len(got.Candidates) != 0 || got.UniqueCount != 0 ||
			got.ConflictCount != 1 || got.DuplicateCount != 1 {
			t.Fatalf("wrong all-conflict result: %+v", got)
		}
	})
}

func TestRAGFusion_TiesAndClip(t *testing.T) {
	t.Run("numeric_id_tie", func(t *testing.T) {
		got := ragFusionRun(t, []retrieval.RankedCandidates{
			{Source: retrieval.SourceES, Candidates: []retrieval.Candidate{
				ragFusionCandidate(10, 9), ragFusionCandidate(2, 8),
			}},
			{Source: retrieval.SourceMilvus, Candidates: []retrieval.Candidate{
				ragFusionCandidate(2, 0.9), ragFusionCandidate(10, 0.8),
			}},
		}, retrieval.HybridOptions{})
		if len(got.Candidates) != 2 ||
			got.Candidates[0].Candidate.Chunk.ChunkID != 2 ||
			got.Candidates[1].Candidate.Chunk.ChunkID != 10 {
			t.Fatal("ties were not ordered by numeric chunk ID")
		}
		ragFusionScore(t, got.Candidates[0].RRFScore,
			got.Candidates[1].RRFScore)
	})

	t.Run("five_hundred_to_two_hundred_fifty", func(t *testing.T) {
		es := retrieval.RankedCandidates{
			Source:     retrieval.SourceES,
			Candidates: make([]retrieval.Candidate, 250),
		}
		mv := retrieval.RankedCandidates{
			Source:     retrieval.SourceMilvus,
			Candidates: make([]retrieval.Candidate, 250),
		}
		for i := 0; i < 250; i++ {
			es.Candidates[i] = ragFusionCandidate(uint64(i+1), 1)
			mv.Candidates[i] = ragFusionCandidate(uint64(1001+i), 0.5)
		}

		got := ragFusionRun(t, []retrieval.RankedCandidates{mv, es},
			retrieval.HybridOptions{TopK: 50})
		if got.RawCount != 500 || got.UniqueCount != 500 ||
			got.DuplicateCount != 0 || got.ConflictCount != 0 ||
			got.ClippedCount != 250 || len(got.Candidates) != 250 {
			t.Fatalf("wrong clipping counts: %+v", got)
		}

		for i, item := range got.Candidates {
			rank := i/2 + 1
			wantID := uint64(rank)
			if i%2 == 1 {
				wantID += 1000
			}
			if item.Candidate.Chunk.ChunkID != wantID {
				t.Fatalf("position %d: ID=%d want=%d",
					i, item.Candidate.Chunk.ChunkID, wantID)
			}
			ragFusionScore(t, item.RRFScore, 1.0/float64(60+rank))
		}
	})

	t.Run("do_not_clip_to_top_k_before_hydration", func(t *testing.T) {
		es := retrieval.RankedCandidates{Source: retrieval.SourceES}
		mv := retrieval.RankedCandidates{Source: retrieval.SourceMilvus}
		for i := 0; i < 5; i++ {
			es.Candidates = append(es.Candidates,
				ragFusionCandidate(uint64(i+1), 1))
			mv.Candidates = append(mv.Candidates,
				ragFusionCandidate(uint64(i+101), 0.5))
		}
		got := ragFusionRun(t, []retrieval.RankedCandidates{es, mv},
			retrieval.HybridOptions{TopK: 1})
		if len(got.Candidates) != 10 || got.ClippedCount != 0 {
			t.Fatal("candidate pool was prematurely reduced to Top-K")
		}
	})
}

func TestRAGFusion_InvalidLists(t *testing.T) {
	es := retrieval.RankedCandidates{
		Source:     retrieval.SourceES,
		Candidates: []retrieval.Candidate{ragFusionCandidate(1, 1)},
	}
	mv := retrieval.RankedCandidates{
		Source:     retrieval.SourceMilvus,
		Candidates: []retrieval.Candidate{ragFusionCandidate(2, 0.5)},
	}

	tests := []struct {
		name  string
		lists []retrieval.RankedCandidates
	}{
		{"nil", nil},
		{"empty", []retrieval.RankedCandidates{}},
		{"three_sources", []retrieval.RankedCandidates{es, mv, es}},
		{"duplicate_source", []retrieval.RankedCandidates{es, es}},
		{"unknown_source", []retrieval.RankedCandidates{
			{Source: "unknown", Candidates: es.Candidates},
		}},
		{"invalid_second_source", []retrieval.RankedCandidates{
			es, {Source: retrieval.SourceMilvus},
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := retrieval.FuseCandidates(
				context.Background(), tt.lists,
				retrieval.HybridOptions{}, ragFusionAt(),
			)
			if !errors.Is(err, retrieval.ErrInvalidCandidates) ||
				!reflect.DeepEqual(got, retrieval.FusionResult{}) {
				t.Fatalf("got=%+v error=%v", got, err)
			}
		})
	}

	t.Run("successful_empty_source", func(t *testing.T) {
		got := ragFusionRun(t, []retrieval.RankedCandidates{
			{Source: retrieval.SourceES, Candidates: []retrieval.Candidate{}},
		}, retrieval.HybridOptions{})
		if len(got.Candidates) != 0 || got.RawCount != 0 ||
			got.UniqueCount != 0 || got.ClippedCount != 0 {
			t.Fatalf("wrong successful empty result: %+v", got)
		}
	})
}

func TestRAGFusion_Cancellation(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, stop := context.WithDeadline(
		context.Background(), time.Now().Add(-time.Second),
	)
	defer stop()

	list := retrieval.RankedCandidates{
		Source:     retrieval.SourceES,
		Candidates: []retrieval.Candidate{ragFusionCandidate(1, 1)},
	}

	for _, tt := range []struct {
		name string
		ctx  context.Context
		want error
	}{
		{"canceled", canceled, context.Canceled},
		{"expired", expired, context.DeadlineExceeded},
	} {
		t.Run(tt.name, func(t *testing.T) {
			frozen, err := retrieval.FreezeRankedCandidates(
				tt.ctx, list, retrieval.HybridOptions{}, ragFusionAt(),
			)
			if !errors.Is(err, tt.want) ||
				!reflect.DeepEqual(frozen, retrieval.RankedCandidates{}) {
				t.Fatalf("freeze=%+v error=%v", frozen, err)
			}

			fused, err := retrieval.FuseCandidates(
				tt.ctx, []retrieval.RankedCandidates{list},
				retrieval.HybridOptions{}, ragFusionAt(),
			)
			if !errors.Is(err, tt.want) ||
				!reflect.DeepEqual(fused, retrieval.FusionResult{}) {
				t.Fatalf("fusion=%+v error=%v", fused, err)
			}
		})
	}
}
