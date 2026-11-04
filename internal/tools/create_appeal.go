package tools

import (
	"context"
	"errors"
	"sync"
	"time"

	"video-support-agent/internal/model"
	"video-support-agent/internal/service"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

const CreateAppealTicketName = "create_appeal_ticket"

var (
	ErrCreateAlreadyAttempted = errors.New("create intent already attempted")
	ErrWriteAuditRequired     = errors.New("durable write audit required")
)

type AppealCreator interface {
	Prepare(context.Context, uint64, uint64, service.CreateAppealInput) (*service.PreparedAppeal, error)
	Create(context.Context, uint64, uint64, service.CreateAppealInput, service.AppealConfirmationInput) (*service.AppealCreateResult, error)
}

type CreateWriteAudit interface {
	BeforeCreate(context.Context, service.AppealConfirmationBinding) error
}

type ConfirmedAppeal struct {
	Input        service.CreateAppealInput       `json:"-"`
	Confirmation service.AppealConfirmationInput `json:"-"`
}

type CreateAppealTicketTool struct {
	session   *RuleSession
	creator   AppealCreator
	audit     CreateWriteAudit
	confirmed *ConfirmedAppeal

	mu        sync.Mutex
	attempted bool
	proposal  *service.PreparedAppeal
	saved     *service.AppealCreateResult
	effect    string
}

var _ tool.InvokableTool = (*CreateAppealTicketTool)(nil)
var _ AppealCreator = (*service.AppealTicketService)(nil)

func NewCreateAppealTicketTool(session *RuleSession, creator AppealCreator, audit CreateWriteAudit, confirmed *ConfirmedAppeal) (*CreateAppealTicketTool, error) {
	if session == nil || creator == nil {
		return nil, fail(ErrConfiguration)
	}
	if err := session.caller.validate(); err != nil {
		return nil, fail(err)
	}

	t := &CreateAppealTicketTool{
		session: session,
		creator: creator,
		audit:   audit,
		effect:  "none",
	}

	if confirmed != nil {
		copy := *confirmed
		input, err := service.NormalizeCreateAppealInput(copy.Input)
		if err != nil {
			return nil, fail(err)
		}
		copy.Input = input

		if !validToolConfirmationToken(copy.Confirmation.Token) {
			return nil, fail(service.ErrAppealConfirmationRequired)
		}
		if _, err := confirmationBinding(
			session.caller, copy,
		); err != nil {
			return nil, fail(err)
		}
		t.confirmed = &copy
	}

	return t, nil
}

func confirmationBinding(caller Caller, confirmed ConfirmedAppeal) (service.AppealConfirmationBinding, error) {
	hash, err := service.AppealRequestHash(
		caller.conversationID, confirmed.Input,
	)
	if err != nil {
		return service.AppealConfirmationBinding{}, err
	}

	binding := service.AppealConfirmationBinding{
		UserID:         caller.userID,
		ConversationID: caller.conversationID,
		IdempotencyKey: confirmed.Confirmation.IdempotencyKey,
		RequestHash:    hash,
		PolicyDigest:   confirmed.Confirmation.PolicyDigest,
	}
	return binding, service.ValidateAppealConfirmationBinding(binding)
}

func validToolConfirmationToken(token string) bool {
	if len(token) == 0 || len(token) > 8192 {
		return false
	}
	for i := range token {
		if token[i] < 0x21 || token[i] > 0x7e {
			return false
		}
	}
	return true
}

func (t *CreateAppealTicketTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return ruleInfo(
		ctx,
		CreateAppealTicketName,
		"提出创建本项目申诉工单。普通模型调用只准备确认，不写工单；"+
			"必须由用户明确确认，人工复核不代表平台受理。",
		`{
			"type":"object",
			"properties":{
				"subject_type":{
					"type":"string",
					"enum":["account","content","other"]
				},
				"subject_ref":{
					"type":"string",
					"minLength":1,
					"maxLength":128
				},
				"reason":{
					"type":"string",
					"minLength":1,
					"maxLength":2000
				},
				"intake_mode":{
					"type":"string",
					"enum":["standard","manual_review"]
				}
			},
			"required":[
				"subject_type","subject_ref","reason","intake_mode"
			],
			"additionalProperties":false
		}`,
	)
}

func decodeCreateAppeal(raw string) (service.CreateAppealInput, error) {
	var input service.CreateAppealInput
	if err := decodeObject(raw, &input, "subject_type", "subject_ref", "reason", "intake_mode"); err != nil {
		return input, err
	}

	return service.NormalizeCreateAppealInput(input)
}

func (t *CreateAppealTicketTool) InvokableRun(ctx context.Context, raw string, _ ...tool.Option) (string, error) {
	if t == nil || t.session == nil || t.creator == nil {
		return "", fail(ErrConfiguration)
	}
	if !t.mu.TryLock() {
		return "", fail(ErrCreateAlreadyAttempted)
	}
	defer t.mu.Unlock()

	if t.attempted {
		return "", fail(ErrCreateAlreadyAttempted)
	}
	t.attempted = true

	if err := ctx.Err(); err != nil {
		return "", fail(err)
	}
	input, err := decodeCreateAppeal(raw)
	if err != nil {
		return "", fail(err)
	}

	work, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if err := t.session.authorize(work); err != nil {
		return "", fail(err)
	}

	if t.confirmed == nil {
		proposal, err := t.creator.Prepare(work, t.session.caller.userID, t.session.caller.conversationID, input)
		if err != nil {
			return "", fail(err)
		}

		proposal, err = checkedProposal(
			t.session.caller, input, proposal,
		)
		if err != nil {
			return "", fail(err)
		}
		if err := work.Err(); err != nil {
			return "", fail(err)
		}

		t.proposal = proposal
		return `{"decision":"confirmation_required","business_effect":"none"}`, nil
	}

	if input != t.confirmed.Input {
		return "", fail(service.ErrAppealPreconditionChanged)
	}

	binding, err := confirmationBinding(
		t.session.caller, *t.confirmed,
	)
	if err != nil {
		return "", fail(err)
	}

	if t.audit == nil {
		return "", fail(ErrWriteAuditRequired)
	}
	if err := t.audit.BeforeCreate(work, binding); err != nil {
		return "", fail(errors.Join(ErrWriteAuditRequired, err))
	}
	if err := work.Err(); err != nil {
		return "", fail(err)
	}

	result, err := t.creator.Create(work, binding.UserID, binding.ConversationID, input, t.confirmed.Confirmation)
	if err != nil {
		if errors.Is(err, service.ErrAppealOutcomeUnknown) {
			t.effect = "unknown"
		}
		return "", fail(err)
	}

	expected := model.AppealTicketSubmitted
	if input.IntakeMode == model.AppealIntakeManualReview {
		expected = model.AppealTicketPendingReview
	}

	if result == nil || result.TicketID == 0 ||
		result.InitialStatus != expected ||
		result.CreatedAt.IsZero() ||
		result.CreatedAt.UTC().Year() < 1000 ||
		result.CreatedAt.UTC().Year() > 9999 ||
		!result.CreatedAt.Equal(
			result.CreatedAt.Truncate(time.Millisecond),
		) {
		t.effect = "unknown"
		return "", fail(service.ErrAppealOutcomeUnknown)
	}

	copy := *result
	copy.CreatedAt = copy.CreatedAt.UTC()

	t.saved, t.effect = &copy, "committed"

	return encodeResult(struct {
		Decision       string                      `json:"decision"`
		BusinessEffect string                      `json:"business_effect"`
		Ticket         *service.AppealCreateResult `json:"ticket"`
	}{
		"ticket_created",
		"committed",
		&copy,
	})
}

func checkedProposal(caller Caller, input service.CreateAppealInput, p *service.PreparedAppeal) (*service.PreparedAppeal, error) {
	if p == nil || p.Input != input ||
		!validToolConfirmationToken(p.Token) {
		return nil, ErrInvalidResult
	}

	copy := *p
	snapshot, err := service.NormalizeAppealSnapshot(p.Eligibility)
	if err != nil {
		return nil, ErrInvalidResult
	}
	copy.Eligibility = snapshot

	status, err := service.AppealInitialStatus(
		snapshot, input.IntakeMode,
	)
	if err != nil || status != p.InitialStatus {
		return nil, ErrInvalidResult
	}

	digest, err := service.AppealPolicyDigest(snapshot)
	if err != nil {
		return nil, ErrInvalidResult
	}

	expected, err := confirmationBinding(caller, ConfirmedAppeal{
		Input: input,
		Confirmation: service.AppealConfirmationInput{
			IdempotencyKey: p.Binding.IdempotencyKey,
			PolicyDigest:   digest,
			Token:          p.Token,
		},
	})

	if err != nil || expected != p.Binding ||
		p.IssuedAt.IsZero() ||
		p.IssuedAt.UTC().Year() < 1000 ||
		p.ExpiresAt.UTC().Year() > 9999 ||
		!p.IssuedAt.Equal(snapshot.CheckedAt) ||
		!p.ExpiresAt.Equal(
			p.IssuedAt.Add(service.AppealConfirmationLifetime),
		) {
		return nil, ErrInvalidResult
	}

	return &copy, nil
}

func (t *CreateAppealTicketTool) Proposal() *service.PreparedAppeal {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.proposal == nil {
		return nil
	}
	copy := *t.proposal
	copy.Eligibility, _ = service.NormalizeAppealSnapshot(
		copy.Eligibility,
	)
	return &copy
}

func (t *CreateAppealTicketTool) BusinessResult() (*service.AppealCreateResult, string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.saved == nil {
		return nil, t.effect
	}
	copy := *t.saved
	return &copy, t.effect
}
