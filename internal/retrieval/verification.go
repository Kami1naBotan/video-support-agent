package retrieval

import (
	"context"
	"errors"
	"time"
)

var (
	ErrInvalidCandidates  = errors.New("invalid retrieval candidates")
	ErrVerificationFailed = errors.New("current-state verification failed")
)

type CurrentVerifier interface {
	VerifyCurrent(ctx context.Context, candidates []Candidate, at time.Time) (map[uint64]struct{}, error)
}
