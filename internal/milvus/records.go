package milvus

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"video-support-agent/internal/embedding"
	indexing "video-support-agent/internal/index"
	"video-support-agent/internal/vectorization"
)

type Record struct {
	ChunkID             string    `json:"chunk_id"`
	DocumentID          string    `json:"document_id"`
	VersionID           string    `json:"version_id"`
	ArticleID           string    `json:"article_id"`
	Category            string    `json:"category"`
	PublicationRevision string    `json:"publication_revision"`
	Version             string    `json:"version"`
	Embedding           []float32 `json:"embedding"`
	EffectiveAt         int64     `json:"effective_at"`
	HasExpiresAt        bool      `json:"has_expires_at"`
	ExpiresAt           int64     `json:"expires_at"`
}

type PreparedImport struct {
	SourceDatabase   string
	SourceServerUUID string
	CapturedAt       time.Time
	Identity         embedding.ModelIdentity
	Records          []Record
}

func PrepareImport(ctx context.Context, snapshot *vectorization.Snapshot, expectedDigest string) (*PreparedImport, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if snapshot == nil {
		return nil, fmt.Errorf("%w: vectorized snapshot is required", ErrInput)
	}
	if len(snapshot.Chunks) == 0 {
		return nil, fmt.Errorf("%w: empty import is not allowed", ErrInput)
	}
	if len(snapshot.Chunks) > indexing.MaxExportDocuments {
		return nil, fmt.Errorf("%w: too many records", ErrInput)
	}
	if err := checkIdentity(snapshot.Identity, expectedDigest); err != nil {
		return nil, err
	}

	source := &indexing.Snapshot{
		SourceDatabase:   snapshot.SourceDatabase,
		SourceServerUUID: snapshot.SourceServerUUID,
		CapturedAt:       snapshot.CapturedAt,
		Documents:        make([]indexing.Document, len(snapshot.Chunks)),
	}
	for i, chunk := range snapshot.Chunks {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		source.Documents[i] = chunk.Source
	}

	prepared, err := indexing.PrepareSnapshot(ctx, source)
	if err != nil {
		return nil, fmt.Errorf("validate import source: %w", err)
	}

	result := &PreparedImport{
		SourceDatabase:   prepared.SourceDatabase,
		SourceServerUUID: prepared.SourceServerUUID,
		CapturedAt:       prepared.CapturedAt,
		Identity:         snapshot.Identity,
		Records:          make([]Record, len(prepared.Documents)),
	}

	for i, encoded := range prepared.Documents {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		var document indexing.Document
		if err := json.Unmarshal(encoded.JSON, &document); err != nil {
			return nil, fmt.Errorf("%w: invalid prepared source at index %d", ErrInput, i)
		}
		if document.ChunkID != encoded.ID || document.ChunkID != snapshot.Chunks[i].Source.ChunkID {
			return nil, fmt.Errorf("%w: source identity differs at index %d", ErrInput, i)
		}
		if len(document.Version) > 256 {
			return nil, fmt.Errorf("%w: chunk_id=%s version exceeds 256 bytes", ErrInput, document.ChunkID)
		}

		vector := snapshot.Chunks[i].Vector
		if len(vector) != embedding.Dimensions {
			return nil, fmt.Errorf("%w: chunk_id=%s has invalid vector dimensions", ErrInput, document.ChunkID)
		}

		raw := make([]float64, len(vector))
		for j, value := range vector {
			raw[j] = float64(value)
		}
		vectors, err := embedding.ValidateVectors([][]float64{raw}, 1)
		if err != nil {
			return nil, fmt.Errorf("%w: chunk_id=%s vector validation failed: %w", ErrInput, document.ChunkID, err)
		}

		record := Record{
			ChunkID:             document.ChunkID,
			DocumentID:          document.DocumentID,
			VersionID:           document.VersionID,
			ArticleID:           document.ArticleID,
			Category:            document.CategoryID,
			PublicationRevision: document.PublicationRevision,
			Version:             document.Version,
			Embedding:           vectors[0],
			EffectiveAt:         document.EffectiveAt.UnixMilli(),
			HasExpiresAt:        document.ExpiresAt != nil,
			ExpiresAt:           0,
		}
		if document.ExpiresAt != nil {
			record.ExpiresAt = document.ExpiresAt.UnixMilli()
		}

		result.Records[i] = record
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func checkIdentity(identity embedding.ModelIdentity, expectedDigest string) error {
	digest, err := hex.DecodeString(expectedDigest)
	if err != nil || len(digest) != 32 || expectedDigest != strings.ToLower(expectedDigest) {
		return fmt.Errorf("%w: invalid expected model digest", ErrInput)
	}

	if identity.Model != embedding.ModelName || identity.Digest != expectedDigest || identity.Dimensions != embedding.Dimensions || identity.ContextLength != embedding.ContextLength || identity.InputPolicy != embedding.InputPolicy {
		return fmt.Errorf("%w: import identity differs from configured contract", embedding.ErrModelMismatch)
	}

	if !utf8.ValidString(identity.ServerVersion) || identity.ServerVersion == "" || identity.ServerVersion != strings.TrimSpace(identity.ServerVersion) {
		return fmt.Errorf("%w: invalid recorded server version", ErrInput)
	}

	return nil
}
