package retrieval

import (
	"errors"
	"time"
)

const HybridSourceTimeout = 8 * time.Second

var ErrHybridUnavailable = errors.New("all hybrid retrieval sources are unavailable")

type SourceStatus string

const (
	SourceRunning   SourceStatus = "running"
	SourceSucceeded SourceStatus = "success"
	SourceEmpty     SourceStatus = "empty"
	SourceFailed    SourceStatus = "failed"
)

type SourceErrorKind string

const (
	SourceErrorNone              SourceErrorKind = ""
	SourceErrorTimeout           SourceErrorKind = "timeout"
	SourceErrorCanceled          SourceErrorKind = "canceled"
	SourceErrorInvalidCandidates SourceErrorKind = "invalid_candidates"
	SourceErrorIndexNotReady     SourceErrorKind = "index_not_ready"
	SourceErrorDependency        SourceErrorKind = "dependency"
)

type SourceReport struct {
	Source        RetrievalSource
	Status        SourceStatus
	Duration      time.Duration
	RawCount      int
	RawCountKnown bool
	ErrorKind     SourceErrorKind
}

type ParallelCandidateResult struct {
	Lists    []RankedCandidates
	Sources  [2]SourceReport
	Degraded bool
	Duration time.Duration
}
