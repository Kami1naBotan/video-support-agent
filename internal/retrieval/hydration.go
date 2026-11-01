package retrieval

import (
	"context"
	"errors"
	"time"
)

var (
	ErrHydrationFailed = errors.New("rule hydration failed")
	ErrHydrationLimit  = errors.New("rule hydration exceeds read budget")
)

const (
	MaxHydrationRowBytes   = 1 << 20
	MaxHydrationTotalBytes = 8 << 20
)

type HydratedChunk struct {
	Chunk          RuleChunk
	ArticleContent string
}

type CurrentHydrator interface {
	HydrateCurrent(ctx context.Context, candidates []Candidate, at time.Time) (map[uint64]HydratedChunk, error)
}
