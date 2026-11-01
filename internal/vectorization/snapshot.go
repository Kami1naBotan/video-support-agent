package vectorization

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"video-support-agent/internal/embedding"
	indexing "video-support-agent/internal/index"
)

type BatchEmbedder interface {
	EmbedWithIdentity(ctx context.Context, texts []string) (embedding.BatchResult, error)
}

var _ BatchEmbedder = (*embedding.Client)(nil)

type Chunk struct {
	Source indexing.Document
	Vector []float32
}

type Snapshot struct {
	SourceDatabase   string
	SourceServerUUID string
	CapturedAt       time.Time
	Identity         embedding.ModelIdentity
	Chunks           []Chunk
}

func Vectorize(ctx context.Context, source *indexing.Snapshot, embedder BatchEmbedder) (*Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, ok := ctx.Deadline(); !ok {
		return nil, fmt.Errorf("%w: vectorization requires an overall deadline", embedding.ErrInput)
	}
	if embedder == nil {
		return nil, fmt.Errorf("%w: embedder is required", embedding.ErrInput)
	}

	result, err := prepare(ctx, source)
	if err != nil {
		return nil, err
	}

	for start := 0; start < len(result.Chunks); {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		end := start
		totalBytes := 0
		for end < len(result.Chunks) && end-start < embedding.MaxBatchItems {
			size := len(result.Chunks[end].Source.Content)
			if size > embedding.MaxBatchTextBytes-totalBytes {
				break
			}
			totalBytes += size
			end++
		}

		if end == start {
			return nil, fmt.Errorf("%w: chunk_id=%s cannot fit in a batch", embedding.ErrInputTooLarge, result.Chunks[start].Source.ChunkID)
		}

		chunks := result.Chunks[start:end]
		texts := make([]string, len(chunks))
		for i, chunk := range chunks {
			texts[i] = chunk.Source.Content
		}

		batch, err := embedder.EmbedWithIdentity(ctx, texts)
		if err != nil {
			if contextErr := ctx.Err(); contextErr != nil {
				return nil, contextErr
			}

			if errors.Is(err, embedding.ErrContextLength) {
				if len(chunks) == 1 {
					return nil, fmt.Errorf("chunk_id=%s: %w", chunks[0].Source.ChunkID, embedding.ErrContextLength)
				}
				return nil, diagnoseContextLength(ctx, embedder, chunks)
			}

			return nil, fmt.Errorf("embed chunk_ids=%s: %w", chunkIDs(chunks), err)
		}

		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := validateIdentity(batch.Identity); err != nil {
			return nil, fmt.Errorf("chunk_ids=%s: %w", chunkIDs(chunks), err)
		}

		if start == 0 {
			result.Identity = batch.Identity
		} else if result.Identity != batch.Identity {
			return nil, fmt.Errorf("%w: model identity changed between batches", embedding.ErrModelMismatch)
		}

		vectors, err := validateAndCopyVectors(batch.Vectors, len(chunks))
		if err != nil {
			return nil, fmt.Errorf("chunk_ids=%s: %w", chunkIDs(chunks), err)
		}

		for i, vector := range vectors {
			result.Chunks[start+i].Vector = vector
		}
		start = end
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func prepare(ctx context.Context, source *indexing.Snapshot) (*Snapshot, error) {
	prepared, err := indexing.PrepareSnapshot(ctx, source)
	if err != nil {
		return nil, fmt.Errorf("prepare vectorization snapshot: %w", err)
	}
	if len(prepared.Documents) == 0 {
		return nil, fmt.Errorf("%w: vectorization snapshot is empty", embedding.ErrInput)
	}

	result := &Snapshot{
		SourceDatabase:   prepared.SourceDatabase,
		SourceServerUUID: prepared.SourceServerUUID,
		CapturedAt:       prepared.CapturedAt,
		Chunks:           make([]Chunk, len(prepared.Documents)),
	}

	for i, encoded := range prepared.Documents {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		var document indexing.Document
		if err := json.Unmarshal(encoded.JSON, &document); err != nil {
			return nil, fmt.Errorf("%w: cannot decode prepared document at index %d", indexing.ErrInvalidSource, i)
		}
		if document.ChunkID != encoded.ID {
			return nil, fmt.Errorf("%w: prepared document identity differs at index %d", indexing.ErrInvalidSource, i)
		}

		if _, err := embedding.PrepareInputs([]string{document.Content}); err != nil {
			return nil, fmt.Errorf("chunk_id=%s: %w", document.ChunkID, err)
		}

		if len(document.Version) > 256 {
			return nil, fmt.Errorf("%w: chunk_id=%s version exceeds 256 bytes", embedding.ErrInput, document.ChunkID)
		}

		result.Chunks[i].Source = document
	}

	return result, nil
}

func validateIdentity(identity embedding.ModelIdentity) error {
	if identity.Model != embedding.ModelName || identity.Dimensions != embedding.Dimensions || identity.ContextLength != embedding.ContextLength || identity.InputPolicy != embedding.InputPolicy {
		return fmt.Errorf("%w: incompatible batch identity", embedding.ErrModelMismatch)
	}

	digest, err := hex.DecodeString(identity.Digest)
	if err != nil || len(digest) != 32 || identity.Digest != strings.ToLower(identity.Digest) {
		return fmt.Errorf("%w: invalid batch model digest", embedding.ErrModelMismatch)
	}

	if !utf8.ValidString(identity.ServerVersion) || identity.ServerVersion == "" || identity.ServerVersion != strings.TrimSpace(identity.ServerVersion) {
		return fmt.Errorf("%w: invalid batch server version", embedding.ErrResponse)
	}
	return nil
}

func validateAndCopyVectors(vectors [][]float32, expectedCount int) ([][]float32, error) {
	if len(vectors) != expectedCount {
		return nil, fmt.Errorf("%w: batch vector count differs", embedding.ErrResponse)
	}

	raw := make([][]float64, len(vectors))
	for i, vector := range vectors {
		if len(vector) != embedding.Dimensions {
			return nil, fmt.Errorf("%w: batch vector %d has invalid dimensions", embedding.ErrResponse, i)
		}

		raw[i] = make([]float64, len(vector))
		for j, value := range vector {
			raw[i][j] = float64(value)
		}
	}

	return embedding.ValidateVectors(raw, expectedCount)
}

func diagnoseContextLength(ctx context.Context, embedder BatchEmbedder, chunks []Chunk) error {
	for _, chunk := range chunks {
		if err := ctx.Err(); err != nil {
			return err
		}

		_, err := embedder.EmbedWithIdentity(ctx, []string{chunk.Source.Content})
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
		if errors.Is(err, embedding.ErrContextLength) {
			return fmt.Errorf("chunk_id=%s: %w", chunk.Source.ChunkID, embedding.ErrContextLength)
		}
		if err != nil {
			return fmt.Errorf("batch chunk_ids=%s: %w; diagnostic chunk_id=%s failed: %w", chunkIDs(chunks), embedding.ErrContextLength, chunk.Source.ChunkID, err)
		}

	}

	return fmt.Errorf("batch chunk_ids=%s: %w; not reproduced by individual inputs", chunkIDs(chunks), embedding.ErrContextLength)
}

func chunkIDs(chunks []Chunk) string {
	ids := make([]string, len(chunks))
	for i, chunk := range chunks {
		ids[i] = chunk.Source.ChunkID
	}
	return strings.Join(ids, ",")
}
