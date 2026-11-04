package agent

import (
	"encoding/json"

	"video-support-agent/internal/model"
	"video-support-agent/internal/service"
	"video-support-agent/internal/tools"
)

func (s *chatSession) consume(name, raw string) (bool, error) {
	switch name {
	case tools.SearchRuleName:
		var found tools.RuleSearchResult
		if json.Unmarshal([]byte(raw), &found) != nil {
			return true, executionError("invalid_result")
		}

		s.latestEvidence = found.EvidenceHandle
		if found.EvidenceHandle == "" {
			fallback(s.out, "handoff", "no_evidence")
			return true, nil
		}

		if _, err := s.registry.RuleEvidence(found.EvidenceHandle); err != nil {
			return true, err
		}

	case tools.CheckAppealEligibilityName:
		var result tools.EligibilityResult
		if json.Unmarshal([]byte(raw), &result) != nil {
			return true, executionError("invalid_result")
		}

		switch result.Decision {
		case model.AppealEligibilityNeedsMoreInfo:
			fallback(s.out, "clarify", "needs_more_info")
			s.out.Content = "申诉信息不完整，请补充相关对象标识和申请说明。"
			return true, nil

		case model.AppealEligibilityIneligible:
			fallback(s.out, "handoff", "ineligible")
			s.out.Content = "当前资格检查不允许创建工单。如有异议，建议人工核对适用政策。"
			return true, nil

		case model.AppealEligibilityEligible,
			model.AppealEligibilityManualReview:

		default:
			return true, executionError("invalid_result")
		}

	case tools.CreateAppealTicketName:
		proposal := s.registry.Proposal()
		ticket, effect := s.registry.BusinessResult()

		if proposal == nil || ticket != nil || effect != "none" {
			return true, executionError("invalid_result")
		}

		s.out.Proposal = proposal
		s.out.Decision = "confirmation_required"
		s.out.ReasonCode = "confirmation_required"
		s.out.Content = "已准备本项目工单确认信息，请核对事项和受理方式；确认前不会创建工单。"
		return true, nil

	case tools.QueryTicketStatusName:
		var status tools.TicketStatusResult
		if json.Unmarshal([]byte(raw), &status) != nil {
			return true, executionError("invalid_result")
		}

		s.out.TicketStatus = &service.AgentTicketStatus{
			TicketID:  status.TicketID,
			Status:    status.Status,
			CreatedAt: status.CreatedAt,
			UpdatedAt: status.UpdatedAt,
		}

		s.out.Decision = "ticket_status"
		s.out.ReasonCode = "none"
		s.out.Content = "本项目工单 " + status.TicketID + " 的当前状态为：" + string(status.Status) + "。"
		return true, nil

	case tools.GetRuleDetailName:

	default:
		return true, executionError("protocol_error")
	}

	return false, nil
}
