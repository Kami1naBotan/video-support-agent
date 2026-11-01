package milvus

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"video-support-agent/internal/embedding"
	"video-support-agent/internal/retrieval"
)

type vqEmbedder func(context.Context, []string) (embedding.BatchResult, error)

func (f vqEmbedder) EmbedWithIdentity(
	ctx context.Context,
	texts []string,
) (embedding.BatchResult, error) {
	return f(ctx, texts)
}

func vqBatch() embedding.BatchResult {
	vector := make([]float32, embedding.Dimensions)
	vector[0] = 1
	return embedding.BatchResult{
		Identity: mvIdentity(),
		Vectors:  [][]float32{vector},
	}
}

func vqTime() time.Time {
	return time.Date(
		2026, 11, 1, 13, 0, 0, 123456789,
		time.FixedZone("UTC+8", 8*60*60),
	)
}

func vqRequireFailure(
	t *testing.T,
	got *PreparedQuery,
	err error,
	want error,
) {
	t.Helper()
	if got != nil || !errors.Is(err, want) {
		t.Fatalf("got=%v err=%v; want nil result and %v", got, err, want)
	}
}

func TestVectorQuerySuccess(t *testing.T) {
	for _, tt := range []struct {
		name       string
		query      string
		normalized string
		limit      int
		wantLimit  int
	}{
		{"default", " \t如何  处理违规投稿？\n", "如何  处理违规投稿？", 0, 10},
		{"maximum", "账号申诉", "账号申诉", 50, 50},
		{"unicode_boundary", strings.Repeat("中", 200), strings.Repeat("中", 200), 1, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := mvContext(t)
			batch := vqBatch()
			calls := 0
			embedder := vqEmbedder(func(
				gotCtx context.Context, texts []string,
			) (embedding.BatchResult, error) {
				calls++
				if gotCtx != ctx {
					t.Fatal("overall context was replaced")
				}
				if !reflect.DeepEqual(texts, []string{tt.normalized}) {
					t.Fatalf("unexpected embedding input: %q", texts)
				}
				return batch, nil
			})

			got, err := PrepareQuery(ctx, tt.query, retrieval.SearchOptions{
				DocumentID: 7,
				CategoryID: 9,
				Limit:      tt.limit,
			}, vqTime(), embedder)
			if err != nil || got == nil {
				t.Fatalf("prepare query: result=%v err=%v", got, err)
			}

			wantOptions := retrieval.SearchOptions{
				DocumentID: 7,
				CategoryID: 9,
				Limit:      tt.wantLimit,
			}
			if calls != 1 || got.Options != wantOptions ||
				got.CandidateLimit != tt.wantLimit*5 ||
				got.Identity != batch.Identity ||
				!got.At.Equal(vqTime().UTC().Truncate(time.Millisecond)) ||
				got.At.Location() != time.UTC ||
				!reflect.DeepEqual(got.Vector, batch.Vectors[0]) {
				t.Fatal("prepared query contract differs")
			}

			batch.Vectors[0][0] = 0.25
			if got.Vector[0] != 1 {
				t.Fatal("returned vector aliases dependency storage")
			}
			got.Vector[1] = 0.5
			if batch.Vectors[0][1] != 0 {
				t.Fatal("dependency vector changed through returned result")
			}
		})
	}
}

func TestVectorQueryPreflight(t *testing.T) {
	for _, name := range []string{
		"empty", "whitespace", "invalid_utf8", "too_many_runes",
		"raw_bytes", "section", "article_number", "highlight",
		"negative_limit", "excess_limit", "zero_time", "invalid_year",
		"no_deadline", "nil_embedder",
	} {
		t.Run(name, func(t *testing.T) {
			ctx := mvContext(t)
			query := "中文问题"
			opts := retrieval.SearchOptions{}
			at := vqTime()
			want := error(retrieval.ErrInvalidQuery)
			calls := 0
			var embedder QueryEmbedder = vqEmbedder(func(
				context.Context, []string,
			) (embedding.BatchResult, error) {
				calls++
				return vqBatch(), nil
			})

			switch name {
			case "empty":
				query = ""
			case "whitespace":
				query = " \t\n"
			case "invalid_utf8":
				query = string([]byte{0xff})
			case "too_many_runes":
				query = strings.Repeat("中", 201)
			case "raw_bytes":
				query = strings.Repeat(" ", embedding.MaxTextBytes+1)
			case "section":
				opts.Section = " "
			case "article_number":
				opts.ArticleNumber = "1"
			case "highlight":
				opts.Highlight = true
			case "negative_limit":
				opts.Limit = -1
			case "excess_limit":
				opts.Limit = 51
			case "zero_time":
				at = time.Time{}
				want = ErrInput
			case "invalid_year":
				at = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
				want = ErrInput
			case "no_deadline":
				ctx = context.Background()
				want = ErrInput
			case "nil_embedder":
				embedder = nil
				want = ErrInput
			}

			got, err := PrepareQuery(ctx, query, opts, at, embedder)
			vqRequireFailure(t, got, err, want)
			if calls != 0 {
				t.Fatal("invalid input reached embedding dependency")
			}
		})
	}
}

func TestVectorQueryResultValidation(t *testing.T) {
	for _, name := range []string{
		"missing_vector", "extra_vector", "wrong_dimensions",
		"zero_vector", "nan", "infinity",
		"model", "digest", "identity_dimensions", "context_length",
		"input_policy", "empty_server", "spaced_server", "invalid_server_utf8",
	} {
		t.Run(name, func(t *testing.T) {
			batch := vqBatch()
			want := error(embedding.ErrResponse)
			switch name {
			case "missing_vector":
				batch.Vectors = nil
			case "extra_vector":
				batch.Vectors = append(batch.Vectors, batch.Vectors[0])
			case "wrong_dimensions":
				batch.Vectors[0] = batch.Vectors[0][:1023]
			case "zero_vector":
				batch.Vectors[0][0] = 0
			case "nan":
				batch.Vectors[0][2] = float32(math.NaN())
			case "infinity":
				batch.Vectors[0][2] = float32(math.Inf(1))
			case "model":
				batch.Identity.Model = "another-model"
				want = embedding.ErrModelMismatch
			case "digest":
				batch.Identity.Digest = strings.Repeat("A", 64)
				want = ErrInput
			case "identity_dimensions":
				batch.Identity.Dimensions = 768
				want = embedding.ErrModelMismatch
			case "context_length":
				batch.Identity.ContextLength = 4096
				want = embedding.ErrModelMismatch
			case "input_policy":
				batch.Identity.InputPolicy = "another-policy"
				want = embedding.ErrModelMismatch
			case "empty_server":
				batch.Identity.ServerVersion = ""
				want = ErrInput
			case "spaced_server":
				batch.Identity.ServerVersion = " 0.40.13"
				want = ErrInput
			case "invalid_server_utf8":
				batch.Identity.ServerVersion = string([]byte{0xff})
				want = ErrInput
			}

			calls := 0
			got, err := PrepareQuery(
				mvContext(t), "中文问题", retrieval.SearchOptions{}, vqTime(),
				vqEmbedder(func(context.Context, []string) (embedding.BatchResult, error) {
					calls++
					return batch, nil
				}),
			)
			vqRequireFailure(t, got, err, want)
			if calls != 1 {
				t.Fatalf("embedding calls=%d, want 1", calls)
			}
		})
	}
}

func TestVectorQueryDependencyErrors(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
	}{
		{"unavailable", embedding.ErrUnavailable},
		{"rejected", embedding.ErrRejected},
		{"context_length", embedding.ErrContextLength},
		{"response", embedding.ErrResponse},
		{"model_mismatch", embedding.ErrModelMismatch},
		{"deadline", context.DeadlineExceeded},
		{"canceled", context.Canceled},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			got, err := PrepareQuery(
				mvContext(t), "中文问题", retrieval.SearchOptions{}, vqTime(),
				vqEmbedder(func(context.Context, []string) (embedding.BatchResult, error) {
					calls++
					// 即使依赖同时返回数据，错误也必须使整次准备失败。
					return vqBatch(), fmt.Errorf("dependency: %w", tt.err)
				}),
			)
			vqRequireFailure(t, got, err, tt.err)
			if calls != 1 {
				t.Fatalf("embedding calls=%d, want 1", calls)
			}
		})
	}
}

func TestVectorQueryCancellation(t *testing.T) {
	for _, name := range []string{
		"before_cancel", "before_deadline", "during_cancel", "during_deadline",
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()

			want := error(context.Canceled)
			wantCalls := 1
			switch name {
			case "before_cancel":
				cancel()
				wantCalls = 0
			case "before_deadline":
				cancel()
				ctx, cancel = context.WithDeadline(
					context.Background(), time.Now().Add(-time.Second),
				)
				defer cancel()
				want = context.DeadlineExceeded
				wantCalls = 0
			case "during_deadline":
				want = context.DeadlineExceeded
			}

			calls := 0
			got, err := PrepareQuery(
				ctx, "中文问题", retrieval.SearchOptions{}, vqTime(),
				vqEmbedder(func(gotCtx context.Context, _ []string) (embedding.BatchResult, error) {
					calls++
					if name == "during_cancel" {
						cancel()
					} else if name == "during_deadline" {
						<-gotCtx.Done()
					}
					return vqBatch(), embedding.ErrUnavailable
				}),
			)
			vqRequireFailure(t, got, err, want)
			if calls != wantCalls {
				t.Fatalf("embedding calls=%d, want %d", calls, wantCalls)
			}
		})
	}
}
