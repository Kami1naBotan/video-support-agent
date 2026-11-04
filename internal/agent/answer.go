package agent

import (
	"video-support-agent/internal/model"
	"video-support-agent/internal/rag"
)

func (s *chatSession) answer() error {
	retrieved, err := s.registry.RuleEvidence(
		s.latestEvidence,
	)
	if err != nil {
		return err
	}

	prepared, err := rag.BuildContext(s.budget.work, s.request.History, s.request.Question, retrieved, nil)
	if err != nil {
		if s.budget.work.Err() != nil {
			return s.budget.work.Err()
		}
		return executionError("invalid_result")
	}

	if prepared.FixedReason != rag.FixedNone {
		fixed, err := rag.FixedReply(s.budget.work, prepared.FixedReason)
		if err != nil {
			return err
		}

		decision := "handoff"
		if fixed.Decision == model.ReplyClarify {
			decision = "clarify"
		}

		code := "no_evidence"
		if decision == "clarify" {
			code = "budget_exhausted"
		}

		fallback(s.out, decision, code)
		s.out.Content = fixed.Content
		return nil
	}

	if s.modelCalls >= 6 {
		fallback(s.out, "handoff", "budget_exhausted")
		return nil
	}

	raw, err := s.generate(s.budget.work, s.executor.models.Answer, prepared.Messages)
	if err != nil {
		return err
	}

	reply, err := rag.ValidateAnswer(s.budget.work, prepared.Evidence, raw)
	if err != nil {
		if s.budget.work.Err() != nil {
			return s.budget.work.Err()
		}

		fallback(s.out, "handoff", "no_evidence")
		return nil
	}

	s.out.Decision = string(reply.Decision)
	if reply.Decision == model.ReplyInsufficient {
		s.out.Decision = "handoff"
	}

	s.out.ReasonCode = "none"
	s.out.Content = reply.Content
	s.out.Citations = reply.Citations

	switch s.out.Decision {
	case "clarify":
		s.out.ReasonCode = "needs_more_info"
	case "handoff":
		s.out.ReasonCode = "no_evidence"
	}

	return nil
}
