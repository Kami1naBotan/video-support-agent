package retrieval

import (
	"context"
	"errors"
	"time"
)

var ErrIndexNotReady = errors.New("retrieval index is not ready")

type Candidate struct {
	Chunk               RuleChunk
	PublicationRevision uint64
}

type CandidateSearcher interface {
	SearchCandidates(ctx context.Context, query string, opts SearchOptions, at time.Time) ([]Candidate, error)
}
