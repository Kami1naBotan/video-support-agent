package evaluation

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	"video-support-agent/internal/retrieval"
)

type RetrievalExecution struct {
	RunID           string               `json:"run_id"`
	CorpusSHA256    string               `json:"corpus_sha256"`
	BindingSHA256   string               `json:"binding_sha256"`
	StartedAt       time.Time            `json:"started_at"`
	DurationMS      int64                `json:"duration_ms"`
	Outcome         string               `json:"outcome"`
	ErrorCode       string               `json:"error_code"`
	ReturnedCount   int                  `json:"returned_count"`
	RawIDs          []RuntimeArticleID   `json:"raw_ids"`
	RawIDsTruncated bool                 `json:"raw_ids_truncated"`
	Observation     RetrievalObservation `json:"observation"`
}

func RunRetrieval(ctx context.Context, retriever retrieval.HybridRetriever, binding *CorpusBinding, runID string, cases []Case, timeout time.Duration) ([]RetrievalExecution, error) {
	if ctx == nil || binding == nil || binding.corpus == nil {
		return nil, fieldError("retrieval_run", "context and binding are required")
	}
	if isNilRetriever(retriever) {
		return nil, fieldError("retrieval_run", "retriever is required")
	}
	if err := validateKey("run_id", runID); err != nil {
		return nil, err
	}
	if timeout <= 0 || timeout > 30*time.Second {
		return nil, fieldError("timeout", "must be positive and at most 30 seconds")
	}
	if err := binding.corpus.ValidateCases(cases); err != nil {
		return nil, err
	}

	options := make([]retrieval.HybridOptions, len(cases))
	for i, c := range cases {
		if c.Suite != SuiteRetrieval {
			return nil, fieldError(fmt.Sprintf("cases[%d].suite", i), "requires retrieval cases")
		}
		if i > 0 && c.TopK != cases[0].TopK {
			return nil, fieldError("top_k", "one run requires a common K")
		}
		option, err := binding.SearchOptions(c)
		if err != nil {
			return nil, fmt.Errorf("cases[%d]: %w", i, err)
		}
		options[i] = option
	}

	executions := make([]RetrievalExecution, 0, len(cases))
	for i, c := range cases {
		executions = append(executions, executeRetrieval(ctx, retriever, binding, runID, c, options[i], timeout))
	}
	return executions, nil
}

func isNilRetriever(retriever retrieval.HybridRetriever) bool {
	if retriever == nil {
		return true
	}
	value := reflect.ValueOf(retriever)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func executeRetrieval(parent context.Context, retriever retrieval.HybridRetriever, binding *CorpusBinding, runID string, c Case, options retrieval.HybridOptions, timeout time.Duration) (execution RetrievalExecution) {
	started := time.Now()
	execution = RetrievalExecution{
		RunID:         runID,
		CorpusSHA256:  binding.corpus.Digest(),
		BindingSHA256: binding.Digest(),
		StartedAt:     started.UTC(),
		Outcome:       "not_started",
		ErrorCode:     "none",
		RawIDs:        make([]RuntimeArticleID, 0),
		Observation: RetrievalObservation{
			CaseID:   c.ID,
			CorpusID: c.CorpusID,
			TopK:     c.TopK,
			Status:   RunFailed,
			Items:    make([]CitationKey, 0),
		},
	}
	defer func() {
		execution.DurationMS = time.Since(started).Milliseconds()
	}()

	if err := parent.Err(); err != nil {
		execution.Observation.Status, execution.ErrorCode = retrievalFailure(err)
		return execution
	}

	callCtx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	result, err := retriever.Retrieve(callCtx, c.Question, options)
	if contextErr := callCtx.Err(); contextErr != nil {
		err = contextErr
	}

	execution.Observation.Degraded = result.Degraded
	execution.ReturnedCount = len(result.Items)
	execution.Outcome = safeHybridOutcome(result.Outcome)

	count := len(result.Items)
	if count > MaxObservedItems {
		count = MaxObservedItems
		execution.RawIDsTruncated = true
	}
	for i := 0; i < count; i++ {
		chunk := result.Items[i].Rule.Chunk
		execution.RawIDs = append(execution.RawIDs, RuntimeArticleID{
			DocumentID: chunk.DocumentID,
			VersionID:  chunk.VersionID,
			ArticleID:  chunk.ArticleID,
		})
	}

	if err != nil {
		execution.Observation.Status, execution.ErrorCode = retrievalFailure(err)
		return execution
	}
	if execution.RawIDsTruncated {
		execution.ErrorCode = "result_too_large"
		return execution
	}
	if !validHybridResult(result) {
		execution.ErrorCode = "invalid_result"
		return execution
	}

	items := make([]CitationKey, 0, len(result.Items))
	for _, item := range result.Items {
		key, err := binding.MatchItem(item)
		if err != nil {
			execution.ErrorCode = "corpus_mismatch"
			return execution
		}
		items = append(items, key)
	}

	execution.Observation.Items = items
	execution.Observation.Status = RunCompleted
	return execution
}

func retrievalFailure(err error) (string, string) {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return RunTimedOut, "timed_out"
	case errors.Is(err, context.Canceled):
		return RunCancelled, "cancelled"
	default:
		return RunFailed, "retrieval_failed"
	}
}

func safeHybridOutcome(outcome retrieval.HybridOutcome) string {
	switch outcome {
	case retrieval.HybridSuccess,
		retrieval.HybridDegraded,
		retrieval.HybridNoEvidence,
		retrieval.HybridDegradedEmpty,
		retrieval.HybridNoUsableEvidence,
		retrieval.HybridFailed:
		return string(outcome)
	default:
		return "unknown"
	}
}

func validHybridResult(result retrieval.HybridResult) bool {
	switch result.Outcome {
	case retrieval.HybridSuccess:
		return len(result.Items) > 0 && !result.Degraded
	case retrieval.HybridDegraded:
		return len(result.Items) > 0 && result.Degraded
	case retrieval.HybridNoEvidence:
		return len(result.Items) == 0 && !result.Degraded
	case retrieval.HybridDegradedEmpty:
		return len(result.Items) == 0 && result.Degraded
	case retrieval.HybridNoUsableEvidence:
		return len(result.Items) == 0
	default:
		return false
	}
}
