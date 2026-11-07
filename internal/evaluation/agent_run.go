package evaluation

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"video-support-agent/internal/service"
	"video-support-agent/internal/tools"
)

type AgentScenario struct {
	CaseID string                     `json:"case_id"`
	Mode   string                     `json:"mode"`
	Input  *service.CreateAppealInput `json:"input"`
}

type AgentAttempt struct {
	AgentRunID     string           `json:"agent_run_id"`
	DurationMS     int64            `json:"duration_ms"`
	ErrorCode      string           `json:"error_code"`
	BusinessEffect string           `json:"business_effect"`
	AuditPending   bool             `json:"audit_pending"`
	Observation    AgentObservation `json:"observation"`
}

type AgentExecution struct {
	Case               Case          `json:"case"`
	OriginalCaseSHA256 string        `json:"original_case_sha256"`
	ScenarioSHA256     string        `json:"scenario_sha256"`
	BindingSHA256      string        `json:"binding_sha256"`
	Mode               string        `json:"mode"`
	UserID             uint64        `json:"user_id"`
	ConversationID     uint64        `json:"conversation_id"`
	Setup              *AgentAttempt `json:"setup,omitempty"`
	Attempt            AgentAttempt  `json:"attempt"`
}

func (r *AgentRunner) Run(ctx context.Context, runID string, cases []Case, scenarios []AgentScenario) ([]AgentExecution, error) {
	if r == nil || r.executor == nil || ctx == nil {
		return nil, fieldError("agent_run", "runner and context are required")
	}
	if err := validateKey("run_id", runID); err != nil {
		return nil, err
	}
	if err := r.binding.corpus.ValidateCases(cases); err != nil {
		return nil, err
	}
	if len(cases) == 0 || len(cases) > MaxCases || len(scenarios) != len(cases) {
		return nil, fieldError("agent_run", "invalid case or scenario count")
	}

	for i, c := range cases {
		s := scenarios[i]
		if c.Suite != SuiteAgent || s.CaseID != c.ID || !oneOf(s.Mode, "chat", "confirm", "ticket_query") {
			return nil, fieldError("agent_run", "case and scenario mismatch")
		}
		if i > 0 && s.Mode != scenarios[0].Mode {
			return nil, fieldError("agent_run", "one batch requires one mode")
		}
		if *c.Filters != (Filters{}) {
			return nil, fieldError("agent_run", "Agent does not accept retrieval filters")
		}
		if s.Mode == "chat" {
			if s.Input != nil {
				return nil, fieldError("agent_run", "chat must not include fixture input")
			}
		} else {
			if s.Input == nil {
				return nil, fieldError("agent_run", "fixture input is required")
			}
			if _, err := service.NormalizeCreateAppealInput(*s.Input); err != nil {
				return nil, fieldError("agent_run", "invalid fixture input")
			}
			raw, err := json.Marshal(s.Input)
			if err != nil || len(raw) > MaxArgumentMatchBytes {
				return nil, fieldError("agent_run", "fixture input exceeds evaluation limit")
			}
		}
		if s.Mode == "ticket_query" {
			if strings.Count(c.Question, "$ticket_id") != 1 {
				return nil, fieldError("agent_run", "query requires one ticket placeholder")
			}
		} else if strings.Contains(c.Question, "$ticket_id") {
			return nil, fieldError("agent_run", "unexpected ticket placeholder")
		}
	}

	results := make([]AgentExecution, 0, len(cases))
	for i, c := range cases {
		results = append(results, r.runCase(ctx, runID, c, scenarios[i]))
	}
	return results, nil
}

func (r *AgentRunner) emptyAttempt(c Case, runID string) AgentAttempt {
	return AgentAttempt{
		ErrorCode:      "not_started",
		BusinessEffect: "none",
		Observation: AgentObservation{
			CaseID:       c.ID,
			CorpusID:     c.CorpusID,
			CorpusSHA256: r.binding.corpus.Digest(),
			RunID:        runID,
			Status:       RunFailed,
			Decision:     "failed",
			Tools:        make([]ObservedTool, 0),
			Citations:    make([]CitationKey, 0),
		},
	}
}

func (r *AgentRunner) runCase(parent context.Context, runID string, c Case, scenario AgentScenario) AgentExecution {
	originalHash, _ := EvidenceHash(c)
	scenarioHash, _ := EvidenceHash(scenario)

	record := AgentExecution{
		Case:               c,
		OriginalCaseSHA256: originalHash,
		ScenarioSHA256:     scenarioHash,
		BindingSHA256:      r.binding.Digest(),
		Mode:               scenario.Mode,
		Attempt:            r.emptyAttempt(c, runID),
	}

	ctx, cancel := context.WithTimeout(parent, 100*time.Second)
	defer cancel()

	owner, err := r.newOwner(ctx, runID, c.ID)
	if err != nil {
		record.Attempt.ErrorCode = "owner_setup_failed"
		if ctx.Err() != nil {
			record.Attempt.Observation.Status, _ = retrievalFailure(ctx.Err())
		}
		return record
	}
	record.UserID = owner.UserID
	record.ConversationID = owner.ConversationID

	switch scenario.Mode {
	case "confirm":
		record.Attempt, _ = r.confirmFixture(ctx, runID, c, owner, *scenario.Input)

	case "ticket_query":
		setup, ticketID := r.confirmFixture(ctx, runID, c, owner, *scenario.Input)
		record.Setup = &setup

		if setup.Observation.Status != RunCompleted || setup.BusinessEffect != "committed" || ticketID == 0 {
			record.Attempt.ErrorCode = "ticket_setup_failed"
			return record
		}

		resolved := resolveTicketCase(c, ticketID)
		if err := ValidateCase(resolved); err != nil {
			record.Attempt.ErrorCode = "resolved_case_invalid"
			return record
		}
		record.Case = resolved
		record.Attempt, _ = r.invoke(ctx, runID, resolved, owner, nil)

	default:
		record.Attempt, _ = r.invoke(ctx, runID, c, owner, nil)
	}
	return record
}

func resolveTicketCase(c Case, ticketID uint64) Case {
	value := strconv.FormatUint(ticketID, 10)
	c.Question = strings.Replace(c.Question, "$ticket_id", value, 1)

	expectedTools := make([]ToolExpectation, len(c.ExpectedTools))
	copy(expectedTools, c.ExpectedTools)
	c.ExpectedTools = expectedTools
	for i := range c.ExpectedTools {
		expected := &c.ExpectedTools[i]
		if expected.Name != tools.QueryTicketStatusName {
			continue
		}

		copyArgs := make(map[string]json.RawMessage, len(expected.ArgumentsMatch))
		for key, raw := range expected.ArgumentsMatch {
			copyArgs[key] = append(json.RawMessage(nil), raw...)
		}
		var placeholder string
		if json.Unmarshal(copyArgs["ticket_id"], &placeholder) == nil && placeholder == "$ticket_id" {
			copyArgs["ticket_id"], _ = json.Marshal(value)
		}
		expected.ArgumentsMatch = copyArgs
	}
	return c
}

func (r *AgentRunner) confirmFixture(ctx context.Context, runID string, c Case, owner service.AgentAuditOwner, input service.CreateAppealInput) (AgentAttempt, uint64) {
	prepareCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	prepared, err := r.appeals.Prepare(prepareCtx, owner.UserID, owner.ConversationID, input)

	prepareContextErr := prepareCtx.Err()
	cancel()

	if err != nil || prepared == nil || prepareContextErr != nil {
		if prepared != nil {
			prepared.Token = ""
		}

		attempt := r.emptyAttempt(c, runID)
		attempt.ErrorCode = "prepare_failed"

		cause := err
		if prepareContextErr != nil {
			cause = prepareContextErr
		}
		if cause != nil {
			status, code := agentFailure(cause)
			attempt.Observation.Status = status
			if status != RunFailed {
				attempt.ErrorCode = code
			}
		}
		return attempt, 0
	}
	defer func() { prepared.Token = "" }()

	request := &service.AgentConfirmedRequest{
		UserID:         owner.UserID,
		ConversationID: owner.ConversationID,
		Input:          prepared.Input,
		Confirmation: service.AppealConfirmationInput{
			IdempotencyKey: prepared.Binding.IdempotencyKey,
			PolicyDigest:   prepared.Binding.PolicyDigest,
			Token:          prepared.Token,
		},
	}
	defer func() { request.Confirmation.Token = "" }()

	return r.invoke(ctx, runID, c, owner, request)
}

func (r *AgentRunner) invoke(parent context.Context, runID string, c Case, owner service.AgentAuditOwner, confirmed *service.AgentConfirmedRequest) (attempt AgentAttempt, ticketID uint64) {
	attempt = r.emptyAttempt(c, runID)
	started := time.Now()
	defer func() {
		attempt.DurationMS = time.Since(started).Milliseconds()
	}()

	ctx, cancel := context.WithTimeout(parent, 45*time.Second)
	defer cancel()

	observedCtx, capture, err := NewToolCapture(ctx, r.binding.corpus)
	if err != nil {
		attempt.ErrorCode = "capture_setup_failed"
		return attempt, 0
	}

	var out *service.AgentOutcome
	if confirmed == nil {
		out, err = r.executor.Execute(
			observedCtx,
			service.AgentRequest{
				UserID:         owner.UserID,
				ConversationID: owner.ConversationID,
				Question:       c.Question,
			},
			nil,
		)
	} else {
		out, err = r.executor.Confirm(observedCtx, *confirmed, nil)
	}

	observedTools, captureErr := capture.Finish(out)
	if out != nil && out.Proposal != nil {
		out.Proposal.Token = ""
	}
	if out != nil && out.Ticket != nil {
		ticketID = out.Ticket.TicketID
	}

	if contextErr := ctx.Err(); contextErr != nil {
		err = contextErr
	}
	r.projectAttempt(&attempt, c, out, observedTools, err, captureErr)
	return attempt, ticketID
}
