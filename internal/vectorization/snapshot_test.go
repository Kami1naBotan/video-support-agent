package vectorization

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"video-support-agent/internal/embedding"
	indexing "video-support-agent/internal/index"
)

type vectorizeEmbedFunc func(context.Context, []string) (embedding.BatchResult, error)

func (f vectorizeEmbedFunc) EmbedWithIdentity(
	ctx context.Context, texts []string,
) (embedding.BatchResult, error) {
	return f(ctx, texts)
}

func vectorizeIdentity() embedding.ModelIdentity {
	return embedding.ModelIdentity{
		Model:         embedding.ModelName,
		Digest:        strings.Repeat("a", 64),
		ServerVersion: "0.40.13",
		Dimensions:    1024,
		ContextLength: 8192,
		InputPolicy:   embedding.InputPolicy,
	}
}

func vectorizeBatch(count, offset int) embedding.BatchResult {
	result := embedding.BatchResult{
		Identity: vectorizeIdentity(),
		Vectors:  make([][]float32, count),
	}
	for i := range result.Vectors {
		result.Vectors[i] = make([]float32, 1024)
		result.Vectors[i][0] = float32(offset + i + 1)
	}
	return result
}

func vectorizeFixture(count int) *indexing.Snapshot {
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	result := &indexing.Snapshot{
		SourceDatabase:   "vectorization_test",
		SourceServerUUID: "test-server-uuid",
		CapturedAt:       at,
		Documents:        make([]indexing.Document, count),
	}
	for i := range result.Documents {
		expires := at.Add(24 * time.Hour)
		result.Documents[i] = indexing.Document{
			ChunkID:             strconv.Itoa(i + 1),
			DocumentID:          "10",
			VersionID:           "20",
			ArticleID:           strconv.Itoa(100 + i),
			CategoryID:          "30",
			PublicationRevision: "2",
			ChunkIndex:          0,
			ArticleOrder:        uint32(i + 1),
			Version:             "test-v1",
			Title:               "明确标记的虚构测试规则",
			SectionPath:         "测试章节",
			ArticleNumber:       strconv.Itoa(i + 1),
			Content:             fmt.Sprintf(" \t虚构测试条款-%d\r\n", i+1),
			SourceURL:           "https://example.invalid/test-fixture",
			Status:              "published",
			IsCurrent:           true,
			EffectiveAt:         at.Add(-time.Hour),
			ExpiresAt:           &expires,
		}
	}
	return result
}

func vectorizeContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func requireVectorizeError(
	t *testing.T, result *Snapshot, err, want error,
) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("error=%v, want=%v", err, want)
	}
	if result != nil {
		t.Fatal("failure returned a partial snapshot")
	}
}

func TestVectorizePreflight(t *testing.T) {
	tests := []struct {
		name string
		edit func(*indexing.Snapshot)
		want error
	}{
		{"empty", func(s *indexing.Snapshot) {
			s.Documents = []indexing.Document{}
		}, embedding.ErrInput},
		{"nil_documents", func(s *indexing.Snapshot) {
			s.Documents = nil
		}, indexing.ErrInvalidSource},
		{"missing_database", func(s *indexing.Snapshot) {
			s.SourceDatabase = ""
		}, indexing.ErrInvalidSource},
		{"invalid_server_identity", func(s *indexing.Snapshot) {
			s.SourceServerUUID = " server "
		}, indexing.ErrInvalidSource},
		{"missing_capture_time", func(s *indexing.Snapshot) {
			s.CapturedAt = time.Time{}
		}, indexing.ErrInvalidSource},
		{"duplicate_last_id", func(s *indexing.Snapshot) {
			s.Documents[16].ChunkID = s.Documents[0].ChunkID
		}, indexing.ErrInvalidSource},
		{"noncanonical_last_id", func(s *indexing.Snapshot) {
			s.Documents[16].ChunkID = "017"
		}, indexing.ErrInvalidSource},
		{"last_not_current", func(s *indexing.Snapshot) {
			s.Documents[16].IsCurrent = false
		}, indexing.ErrInvalidSource},
		{"last_draft", func(s *indexing.Snapshot) {
			s.Documents[16].Status = "draft"
		}, indexing.ErrInvalidSource},
		{"last_future", func(s *indexing.Snapshot) {
			s.Documents[16].EffectiveAt = s.CapturedAt.Add(time.Hour)
		}, indexing.ErrInvalidSource},
		{"last_expired", func(s *indexing.Snapshot) {
			at := s.CapturedAt
			s.Documents[16].ExpiresAt = &at
		}, indexing.ErrInvalidSource},
		{"last_withdrawn", func(s *indexing.Snapshot) {
			at := s.CapturedAt
			s.Documents[16].WithdrawnAt = &at
		}, indexing.ErrInvalidSource},
		{"last_blank_content", func(s *indexing.Snapshot) {
			s.Documents[16].Content = "\t \u3000"
		}, indexing.ErrInvalidSource},
		{"last_invalid_utf8", func(s *indexing.Snapshot) {
			s.Documents[16].Content = string([]byte{0xff})
		}, indexing.ErrInvalidSource},
		{"last_content_over_budget", func(s *indexing.Snapshot) {
			s.Documents[16].Content = strings.Repeat("x", (64<<10)+1)
		}, embedding.ErrInputTooLarge},
		{"last_version_over_budget", func(s *indexing.Snapshot) {
			s.Documents[16].Version = strings.Repeat("版", 86)
		}, embedding.ErrInput},
		{"export_count_limit", func(s *indexing.Snapshot) {
			s.Documents = make([]indexing.Document, indexing.MaxExportDocuments+1)
		}, indexing.ErrExportLimitExceeded},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			source := vectorizeFixture(17)
			tt.edit(source)
			calls := 0
			embedder := vectorizeEmbedFunc(func(
				context.Context, []string,
			) (embedding.BatchResult, error) {
				calls++
				return embedding.BatchResult{}, errors.New("unexpected call")
			})
			got, err := Vectorize(vectorizeContext(t), source, embedder)
			requireVectorizeError(t, got, err, tt.want)
			if calls != 0 {
				t.Fatal("preflight failure reached the embedder")
			}
		})
	}

	for _, mode := range []string{
		"nil_source", "nil_embedder", "no_deadline", "canceled", "expired",
	} {
		t.Run(mode, func(t *testing.T) {
			ctx := vectorizeContext(t)
			source := vectorizeFixture(1)
			calls := 0
			var embedder BatchEmbedder = vectorizeEmbedFunc(func(
				context.Context, []string,
			) (embedding.BatchResult, error) {
				calls++
				return vectorizeBatch(1, 0), nil
			})
			var want error = embedding.ErrInput
			switch mode {
			case "nil_source":
				source = nil
				want = indexing.ErrInvalidSource
			case "nil_embedder":
				embedder = nil
			case "no_deadline":
				ctx = context.Background()
			case "canceled":
				child, cancel := context.WithCancel(ctx)
				cancel()
				ctx = child
				want = context.Canceled
			case "expired":
				child, cancel := context.WithDeadline(ctx, time.Unix(1, 0))
				defer cancel()
				ctx = child
				want = context.DeadlineExceeded
			}
			got, err := Vectorize(ctx, source, embedder)
			requireVectorizeError(t, got, err, want)
			if calls != 0 {
				t.Fatal("invalid operation reached the embedder")
			}
		})
	}
}

func TestVectorizeBatching(t *testing.T) {
	tests := []struct {
		name  string
		count int
		bytes int
		sizes []int
	}{
		{"exact_item_limit", 16, 0, []int{16}},
		{"over_item_limit", 17, 0, []int{16, 1}},
		{"three_batches", 33, 0, []int{16, 16, 1}},
		{"exact_byte_limit", 4, 64 << 10, []int{4}},
		{"over_byte_limit", 5, 64 << 10, []int{4, 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			source := vectorizeFixture(tt.count)
			// 256 字节版本名应被接受。
			source.Documents[0].Version = strings.Repeat("v", 256)
			if tt.bytes != 0 {
				for i := range source.Documents {
					prefix := fmt.Sprintf("中文规则-%d\n", i)
					source.Documents[i].Content =
						prefix + strings.Repeat("x", tt.bytes-len(prefix))
				}
			}
			ctx := vectorizeContext(t)
			deadline, _ := ctx.Deadline()
			var sizes []int
			offset := 0
			embedder := vectorizeEmbedFunc(func(
				callCtx context.Context, texts []string,
			) (embedding.BatchResult, error) {
				d, ok := callCtx.Deadline()
				if !ok || !d.Equal(deadline) {
					t.Fatal("overall deadline changed between batches")
				}
				sizes = append(sizes, len(texts))
				total := 0
				for i, text := range texts {
					total += len(text)
					if offset+i >= len(source.Documents) ||
						text != source.Documents[offset+i].Content {
						t.Fatal("input order or original text changed")
					}
				}
				if len(texts) == 0 || len(texts) > 16 || total > 256<<10 {
					t.Fatal("batch exceeded item/byte budget")
				}
				result := vectorizeBatch(len(texts), offset)
				offset += len(texts)
				return result, nil
			})
			got, err := Vectorize(ctx, source, embedder)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(sizes, tt.sizes) {
				t.Fatalf("batch sizes=%v, want=%v", sizes, tt.sizes)
			}
			if got == nil || len(got.Chunks) != tt.count || offset != tt.count {
				t.Fatal("missing or duplicated chunks")
			}
			if got.SourceDatabase != source.SourceDatabase ||
				got.SourceServerUUID != source.SourceServerUUID ||
				!got.CapturedAt.Equal(source.CapturedAt) ||
				got.Identity != vectorizeIdentity() {
				t.Fatal("snapshot identity changed")
			}
			for i, chunk := range got.Chunks {
				if !reflect.DeepEqual(chunk.Source, source.Documents[i]) {
					t.Fatalf("metadata changed at chunk %d", i)
				}
				if len(chunk.Vector) != 1024 || chunk.Vector[0] != float32(i+1) {
					t.Fatalf("vector binding changed at chunk %d", i)
				}
			}
		})
	}
}

func TestVectorizeOwnership(t *testing.T) {
	source := vectorizeFixture(17)
	want := vectorizeFixture(17)
	shared := make([]float32, 1024)
	calls := 0
	embedder := vectorizeEmbedFunc(func(
		_ context.Context, texts []string,
	) (embedding.BatchResult, error) {
		calls++
		if calls == 1 {
			// 第一次模型调用时修改源数据，验证预检已冻结全部片段。
			source.SourceDatabase = "changed"
			source.Documents[16].Content = "changed"
			source.Documents[16].ChunkID = "999"
			*source.Documents[16].ExpiresAt = source.CapturedAt
		} else if calls == 2 {
			if len(texts) != 1 || texts[0] != want.Documents[16].Content {
				t.Fatal("later batch read mutated source")
			}
		} else {
			t.Fatal("unexpected extra batch")
		}

		// 模拟客户端每次调用复用同一块向量内存。
		shared[0] = float32(calls)
		result := embedding.BatchResult{
			Identity: vectorizeIdentity(),
			Vectors:  make([][]float32, len(texts)),
		}
		for i := range result.Vectors {
			result.Vectors[i] = shared
		}
		return result, nil
	})
	got, err := Vectorize(vectorizeContext(t), source, embedder)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || len(got.Chunks) != 17 || calls != 2 {
		t.Fatal("wrong result or batch count")
	}
	if got.SourceDatabase != want.SourceDatabase {
		t.Fatal("snapshot retained mutable source identity")
	}
	shared[0] = 99
	for i, chunk := range got.Chunks {
		expected := float32(1)
		if i == 16 {
			expected = 2
		}
		if !reflect.DeepEqual(chunk.Source, want.Documents[i]) ||
			chunk.Vector[0] != expected {
			t.Fatalf("snapshot/vector storage leaked at chunk %d", i)
		}
	}
	got.Chunks[0].Vector[0] = 77
	if got.Chunks[1].Vector[0] != 1 {
		t.Fatal("output vectors share storage")
	}
	*got.Chunks[0].Source.ExpiresAt = got.CapturedAt
	if !source.Documents[0].ExpiresAt.Equal(*want.Documents[0].ExpiresAt) {
		t.Fatal("output expiry pointer aliases source")
	}
}

func TestVectorizeInvalidResults(t *testing.T) {
	tests := []struct {
		name string
		edit func(*embedding.BatchResult)
		want error
	}{
		{"model", func(b *embedding.BatchResult) {
			b.Identity.Model = "other"
		}, embedding.ErrModelMismatch},
		{"dimension_identity", func(b *embedding.BatchResult) {
			b.Identity.Dimensions = 768
		}, embedding.ErrModelMismatch},
		{"context_identity", func(b *embedding.BatchResult) {
			b.Identity.ContextLength = 4096
		}, embedding.ErrModelMismatch},
		{"input_policy", func(b *embedding.BatchResult) {
			b.Identity.InputPolicy = "other"
		}, embedding.ErrModelMismatch},
		{"digest_short", func(b *embedding.BatchResult) {
			b.Identity.Digest = "aa"
		}, embedding.ErrModelMismatch},
		{"digest_uppercase", func(b *embedding.BatchResult) {
			b.Identity.Digest = strings.Repeat("A", 64)
		}, embedding.ErrModelMismatch},
		{"digest_nonhex", func(b *embedding.BatchResult) {
			b.Identity.Digest = strings.Repeat("z", 64)
		}, embedding.ErrModelMismatch},
		{"digest_changed_between_batches", func(b *embedding.BatchResult) {
			b.Identity.Digest = strings.Repeat("b", 64)
		}, embedding.ErrModelMismatch},
		{"server_changed_between_batches", func(b *embedding.BatchResult) {
			b.Identity.ServerVersion = "0.40.14"
		}, embedding.ErrModelMismatch},
		{"server_blank", func(b *embedding.BatchResult) {
			b.Identity.ServerVersion = ""
		}, embedding.ErrResponse},
		{"server_whitespace", func(b *embedding.BatchResult) {
			b.Identity.ServerVersion = " 0.40.13"
		}, embedding.ErrResponse},
		{"server_invalid_utf8", func(b *embedding.BatchResult) {
			b.Identity.ServerVersion = string([]byte{0xff})
		}, embedding.ErrResponse},
		{"missing_vector", func(b *embedding.BatchResult) {
			b.Vectors = nil
		}, embedding.ErrResponse},
		{"extra_vector", func(b *embedding.BatchResult) {
			b.Vectors = append(b.Vectors, b.Vectors[0])
		}, embedding.ErrResponse},
		{"wrong_dimension", func(b *embedding.BatchResult) {
			b.Vectors[0] = b.Vectors[0][:1023]
		}, embedding.ErrResponse},
		{"zero_vector", func(b *embedding.BatchResult) {
			b.Vectors[0][0] = 0
		}, embedding.ErrResponse},
		{"nan", func(b *embedding.BatchResult) {
			b.Vectors[0][0] = float32(math.NaN())
		}, embedding.ErrResponse},
		{"infinity", func(b *embedding.BatchResult) {
			b.Vectors[0][0] = float32(math.Inf(1))
		}, embedding.ErrResponse},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			embedder := vectorizeEmbedFunc(func(
				_ context.Context, texts []string,
			) (embedding.BatchResult, error) {
				calls++
				result := vectorizeBatch(len(texts), 0)
				if calls == 2 {
					tt.edit(&result)
				}
				return result, nil
			})
			got, err := Vectorize(
				vectorizeContext(t), vectorizeFixture(17), embedder,
			)
			requireVectorizeError(t, got, err, tt.want)
			if calls != 2 {
				t.Fatalf("calls=%d, want=2", calls)
			}
		})
	}
}

func TestVectorizeFailures(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		cancel bool
		want   error
	}{
		{"unavailable", embedding.ErrUnavailable, false, embedding.ErrUnavailable},
		{"rejected", embedding.ErrRejected, false, embedding.ErrRejected},
		{"response", embedding.ErrResponse, false, embedding.ErrResponse},
		{"deadline", context.DeadlineExceeded, false, context.DeadlineExceeded},
		{"cancel_after_success", nil, true, context.Canceled},
		{"cancel_overrides_service_error", embedding.ErrUnavailable, true, context.Canceled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(vectorizeContext(t))
			defer cancel()
			calls := 0
			embedder := vectorizeEmbedFunc(func(
				_ context.Context, texts []string,
			) (embedding.BatchResult, error) {
				calls++
				if calls == 2 {
					if tt.cancel {
						cancel()
					}
					return vectorizeBatch(len(texts), 16), tt.err
				}
				return vectorizeBatch(len(texts), 0), nil
			})
			got, err := Vectorize(ctx, vectorizeFixture(33), embedder)
			requireVectorizeError(t, got, err, tt.want)
			if calls != 2 {
				t.Fatalf("calls=%d; failure retried or third batch started", calls)
			}
		})
	}
}

func TestVectorizeContextDiagnostics(t *testing.T) {
	tests := []struct {
		name      string
		count     int
		failAt    int
		diagErr   error
		cancel    bool
		wantCalls int
		want      error
		fragment  string
	}{
		{"single_no_retry", 1, 0, nil, false, 1,
			embedding.ErrContextLength, "chunk_id=1"},
		{"locate_second", 17, 2, embedding.ErrContextLength, false, 3,
			embedding.ErrContextLength, "chunk_id=2"},
		{"all_individual_success", 17, 0, nil, false, 17,
			embedding.ErrContextLength, "not reproduced by individual inputs"},
		{"diagnostic_unavailable", 17, 2, embedding.ErrUnavailable, false, 3,
			embedding.ErrUnavailable, "diagnostic chunk_id=2"},
		{"diagnostic_canceled", 17, 1, nil, true, 2,
			context.Canceled, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			source := vectorizeFixture(tt.count)
			ctx, cancel := context.WithCancel(vectorizeContext(t))
			defer cancel()
			calls := 0
			embedder := vectorizeEmbedFunc(func(
				_ context.Context, texts []string,
			) (embedding.BatchResult, error) {
				calls++
				if calls == 1 {
					expected := tt.count
					if expected > 16 {
						expected = 16
					}
					if len(texts) != expected {
						t.Fatal("unexpected initial batch size")
					}
					return embedding.BatchResult{},
						fmt.Errorf("model: %w", embedding.ErrContextLength)
				}

				diagnosticIndex := calls - 2
				if diagnosticIndex >= 16 ||
					len(texts) != 1 ||
					texts[0] != source.Documents[diagnosticIndex].Content {
					t.Fatal("diagnostic exceeded batch or changed input/order")
				}
				if calls-1 == tt.failAt {
					if tt.cancel {
						cancel()
					}
					return vectorizeBatch(1, 0), tt.diagErr
				}
				return vectorizeBatch(1, 0), nil
			})
			got, err := Vectorize(ctx, source, embedder)
			requireVectorizeError(t, got, err, tt.want)
			if calls != tt.wantCalls {
				t.Fatalf("calls=%d, want=%d", calls, tt.wantCalls)
			}
			if tt.fragment != "" && !strings.Contains(err.Error(), tt.fragment) {
				t.Fatalf("missing diagnostic context: %v", err)
			}
			if tt.name == "diagnostic_unavailable" &&
				!errors.Is(err, embedding.ErrContextLength) {
				t.Fatal("diagnostic error lost original context-length error")
			}
			for _, document := range source.Documents {
				if strings.Contains(err.Error(), document.Content) {
					t.Fatal("diagnostic exposed original content")
				}
			}
		})
	}
}
