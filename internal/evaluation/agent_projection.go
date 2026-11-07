package evaluation

import (
	"context"
	"errors"
	"strconv"

	"video-support-agent/internal/model"
	"video-support-agent/internal/service"
)

func agentFailure(err error) (string, string) {
	switch {
	case errors.Is(err, context.Canceled):
		return RunCancelled, "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return RunTimedOut, "timed_out"
	case errors.Is(err, service.ErrAppealOutcomeUnknown):
		return RunUnknown, "outcome_unknown"
	}

	var execution *service.AgentExecutionError
	if errors.As(err, &execution) {
		switch execution.Code {
		case "cancelled":
			return RunCancelled, "cancelled"
		case "timed_out":
			return RunTimedOut, "timed_out"
		case "outcome_unknown":
			return RunUnknown, "outcome_unknown"
		}
	}
	return RunFailed, "agent_failed"
}

func (r *AgentRunner) projectAttempt(attempt *AgentAttempt, c Case, out *service.AgentOutcome, observedTools []ObservedTool, runErr, captureErr error) {
	if out == nil {
		attempt.ErrorCode = "outcome_missing"
		if runErr != nil {
			attempt.Observation.Status, attempt.ErrorCode = agentFailure(runErr)
		}
		return
	}

	if service.ValidateAppealOperationKey(out.RunID) == nil {
		attempt.AgentRunID = out.RunID
	}
	attempt.AuditPending = out.AuditPending
	if oneOf(out.BusinessEffect, "none", "unknown", "committed") {
		attempt.BusinessEffect = out.BusinessEffect
	} else {
		attempt.BusinessEffect = "unknown"
	}

	observation := attempt.Observation
	observation.Status = out.ExecutionStatus
	observation.Decision = out.Decision
	observation.Answer = out.Content
	observation.Tools = observedTools

	attempt.ErrorCode = "none"
	if runErr != nil {
		observation.Status, attempt.ErrorCode = agentFailure(runErr)
	}
	if out.ExecutionStatus == RunUnknown || attempt.BusinessEffect == "unknown" {
		observation.Status = RunUnknown
		attempt.ErrorCode = "outcome_unknown"
	}
	if captureErr != nil {
		observation.Tools = make([]ObservedTool, 0)
		if observation.Status == RunCompleted {
			observation.Status = RunFailed
		}
		attempt.ErrorCode = "capture_failed"
	}

	projectionFailed := len(out.Citations) > MaxObservedItems

	citationCapacity := 0
	if !projectionFailed {
		citationCapacity = len(out.Citations)
	}
	observation.Citations = make([]CitationKey, 0, citationCapacity)

	if !projectionFailed {
		for _, citation := range out.Citations {
			key, err := r.citationKey(citation)
			if err != nil {
				projectionFailed = true
				break
			}
			observation.Citations = append(observation.Citations, key)
		}
	}

	if projectionFailed || validateAgentObservation(c, observation) != nil || (observation.Status == RunCompleted && attempt.AgentRunID == "") {
		attempt.ErrorCode = "invalid_observation"

		switch observation.Status {
		case RunUnknown, RunTimedOut, RunCancelled:
			attempt.Observation.Status = observation.Status
		default:
			attempt.Observation.Status = RunFailed
		}
		return
	}

	if observation.Status != RunCompleted && attempt.ErrorCode == "none" {
		attempt.ErrorCode = "agent_failed"
	}
	attempt.Observation = observation
}

func (r *AgentRunner) citationKey(citation model.RuleCitation) (CitationKey, error) {
	parse := func(raw string) (uint64, error) {
		value, err := strconv.ParseUint(raw, 10, 64)
		if err != nil || value == 0 || strconv.FormatUint(value, 10) != raw {
			return 0, fieldError("citation", "invalid runtime ID")
		}
		return value, nil
	}

	documentID, err := parse(citation.DocumentID)
	if err != nil {
		return CitationKey{}, err
	}
	versionID, err := parse(citation.VersionID)
	if err != nil {
		return CitationKey{}, err
	}
	articleID, err := parse(citation.ArticleID)
	if err != nil {
		return CitationKey{}, err
	}
	categoryID, err := parse(citation.CategoryID)
	if err != nil {
		return CitationKey{}, err
	}

	key, exists := r.binding.Citation(RuntimeArticleID{
		DocumentID: documentID,
		VersionID:  versionID,
		ArticleID:  articleID,
	})
	if !exists {
		return CitationKey{}, fieldError("citation", "outside frozen corpus")
	}

	expected, _ := r.binding.corpus.Article(key)
	if categoryID != r.binding.forward[3][expected.CategoryKey] ||
		!matchesCorpusContent(expected, expected.Content, citation.Title, citation.SourceURL, &citation.EffectiveAt, citation.ExpiresAt) {
		return CitationKey{}, fieldError("citation", "metadata mismatch")
	}
	return key, nil
}
