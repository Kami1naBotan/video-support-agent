package embedding

import (
	"context"
	"errors"
)

const (
	ModelName     = "bge-m3:567m"
	Dimensions    = 1024
	ContextLength = 8192

	InputPolicy = "chunk_content_v1"

	MaxBatchItems     = 16
	MaxTextBytes      = 64 << 10
	MaxBatchTextBytes = 256 << 10
)

var (
	ErrInput         = errors.New("invalid embedding input")
	ErrInputTooLarge = errors.New("embedding input exceeds application budget")
	ErrContextLength = errors.New("embedding input exceeds model context")
	ErrUnavailable   = errors.New("embedding service unavailable")
	ErrRejected      = errors.New("embedding request rejected")
	ErrResponse      = errors.New("invalid embedding response")
	ErrModelMismatch = errors.New("embedding model identity mismatch")
)

type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}
