package retrieval

import (
	"context"
	"errors"
	"time"
)

const (
	HybridTimeout          = 12 * time.Second
	HybridHydrationTimeout = 3 * time.Second
	HybridRerankTimeout    = time.Second

	SourceNotStarted SourceStatus = "not_started"
)

var ErrHybridConfiguration = errors.New("hybrid retrieval is not configured correctly")

type HybridOutcome string

const (
	HybridSuccess          HybridOutcome = "ok"
	HybridDegraded         HybridOutcome = "degraded"
	HybridNoEvidence       HybridOutcome = "no_evidence"
	HybridDegradedEmpty    HybridOutcome = "degraded_empty"
	HybridNoUsableEvidence HybridOutcome = "no_usable_evidence"
	HybridFailed           HybridOutcome = "failed"
)

type HybridStageStatus string

const (
	HybridStageNotRun   HybridStageStatus = "not_run"
	HybridStageSkipped  HybridStageStatus = "skipped"
	HybridStageSuccess  HybridStageStatus = "success"
	HybridStageFallback HybridStageStatus = "fallback"
	HybridStageFailed   HybridStageStatus = "failed"
)

type HybridStageReport struct {
	Status    HybridStageStatus
	Duration  time.Duration
	ErrorKind string
}

type HybridStats struct {
	Sources        [2]SourceReport
	SourceDuration time.Duration

	Fusion    HybridStageReport
	Hydration HybridStageReport
	Rerank    HybridStageReport

	RawCount       int
	DuplicateCount int
	ConflictCount  int
	UniqueCount    int
	ClippedCount   int
	FusedCount     int

	HydratedCount int
	FilteredCount int
	ArticleCount  int
	FinalCount    int
}

type HybridResult struct {
	Items       []HybridItem
	RetrievedAt time.Time
	Outcome     HybridOutcome
	Degraded    bool
	EmptyReason string
	ErrorKind   string
	Stats       HybridStats
	Duration    time.Duration
}

type HybridRetriever interface {
	Retrieve(ctx context.Context, query string, opts HybridOptions) (HybridResult, error)
}
