package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"video-support-agent/internal/config"
	"video-support-agent/internal/confirmation"
	"video-support-agent/internal/model"
	"video-support-agent/internal/retrieval"
	"video-support-agent/internal/service"
	"video-support-agent/internal/tools"
)

const confirmRunID = "00000000-0000-4000-8000-000000000001"
const confirmCallID = "00000000-0000-4000-8000-000000000002"

var confirmAt = time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)

func confirmOK(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func confirmCode(t *testing.T, err error, want string) {
	t.Helper()
	if want == "" {
		confirmOK(t, err)
		return
	}
	var safe *service.AgentExecutionError
	if !errors.As(err, &safe) || safe.Code != want || err.Error() != want {
		t.Fatalf("error=%v; want safe code %s", err, want)
	}
}

// 真实签名器，只记录Verify调用次数。
type countedConfirmation struct {
	*confirmation.AppealSigner
	verifies int
}

func (s *countedConfirmation) Verify(
	ctx context.Context,
	token string,
	b service.AppealConfirmationBinding,
) error {
	s.verifies++
	return s.AppealSigner.Verify(ctx, token, b)
}

type confirmRig struct {
	t                   *testing.T
	now                 time.Time
	signer              *countedConfirmation
	svc                 *service.AppealTicketService
	executor            *ConfirmedExecutor
	request             service.AgentConfirmedRequest
	binding             service.AppealConfirmationBinding
	run                 model.AgentRun
	call                *model.ToolCall
	ticket              *model.AppealTicket
	trace               []string
	events              []service.AgentEvent
	failAt              string
	badRun              string
	badCall             string
	expireInTransaction bool
	cancelAfterCommit   context.CancelFunc
	operationErr        error
	readErr             error
	txCalls             int
	operationReads      int
	finishCall          *service.AgentCallFinish
	finishRun           *service.AgentRunFinish
	finalDeadline       time.Time
}

// 两种GetByID的返回类型不同，因此分别实现接口。
type confirmReadDeps struct {
	rig *confirmRig
}

func (d confirmReadDeps) GetByID(
	ctx context.Context, u, c uint64,
) (*model.Conversation, error) {
	if err := d.rig.CheckConversationOwnership(ctx, u, c); err != nil {
		return nil, err
	}
	return &model.Conversation{ID: c, UserID: u}, nil
}

func (d confirmReadDeps) Retrieve(
	context.Context, string, retrieval.HybridOptions,
) (retrieval.HybridResult, error) {
	d.rig.t.Fatal("confirmed execution invoked retrieval")
	return retrieval.HybridResult{}, nil
}

func (d confirmReadDeps) GetCurrentByDocumentID(
	context.Context, uint64,
) (*service.KnowledgeCurrentDetail, error) {
	d.rig.t.Fatal("confirmed execution read rules")
	return nil, nil
}

func (d confirmReadDeps) GetHistoryArticle(
	context.Context, uint64, uint64,
) (*service.KnowledgeHistoryArticleDetail, error) {
	d.rig.t.Fatal("confirmed execution read history")
	return nil, nil
}

func newConfirmRig(t *testing.T) *confirmRig {
	t.Helper()
	r := &confirmRig{t: t, now: confirmAt}
	signer, err := confirmation.NewAppealSignerWithClock(
		config.AppealConfirmationConfig{
			KeyBase64: base64.StdEncoding.EncodeToString(
				bytes.Repeat([]byte{0x62}, 32),
			),
			Issuer:   "test-confirmed-executor",
			Audience: "test-appeal-create",
		},
		func() time.Time { return r.now },
	)
	confirmOK(t, err)
	r.signer = &countedConfirmation{AppealSigner: signer}

	eligibility, err := service.NewAppealEligibilityService(
		service.NoAppealPolicies{},
	)
	confirmOK(t, err)
	r.svc, err = service.NewAppealTicketServiceWithClock(
		r, eligibility, r.signer, func() time.Time { return r.now },
	)
	confirmOK(t, err)

	input := service.CreateAppealInput{
		SubjectType: model.AppealSubjectAccount,
		SubjectRef:  "test-private-subject",
		Reason:      "test-private-reason",
		IntakeMode:  model.AppealIntakeManualReview,
	}
	p, err := r.svc.Prepare(context.Background(), 7, 9, input)
	confirmOK(t, err)
	r.binding = p.Binding
	r.request = service.AgentConfirmedRequest{
		UserID: 7, ConversationID: 9, Input: p.Input,
		Confirmation: service.AppealConfirmationInput{
			IdempotencyKey: p.Binding.IdempotencyKey,
			PolicyDigest:   p.Binding.PolicyDigest,
			Token:          p.Token,
		},
	}

	d := confirmReadDeps{r}
	r.executor, err = NewConfirmedExecutor(
		tools.RegistryDependencies{
			Conversations: d,
			Retriever:     d,
			Knowledge:     d,
			Appeals:       r.svc,
		},
		r,
		r.svc,
	)
	confirmOK(t, err)
	r.resetAudit()
	return r
}

func (r *confirmRig) resetAudit() {
	r.run = model.AgentRun{
		ID: confirmRunID, UserID: 7, ConversationID: 9,
		ExecutionStatus:   "running",
		BusinessEffect:    "none",
		ReasonCode:        "none",
		MessageSaveStatus: "not_attempted",
	}
	r.call = nil
	r.trace = nil
	r.events = nil
	r.finishCall = nil
	r.finishRun = nil
	r.finalDeadline = time.Time{}
}

func (r *confirmRig) note(
	ctx context.Context, name string, limit time.Duration,
) error {
	r.t.Helper()
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) > limit {
		r.t.Fatalf("%s missing bounded deadline", name)
	}
	r.trace = append(r.trace, name)
	if r.failAt == name {
		return service.ErrAgentAuditUnavailable
	}
	return ctx.Err()
}

func (r *confirmRig) checkOwner(o service.AgentAuditOwner) {
	r.t.Helper()
	if o.UserID != 7 || o.ConversationID != 9 {
		r.t.Fatalf("unexpected owner: %+v", o)
	}
}

func (r *confirmRig) checkFinal(ctx context.Context) {
	r.t.Helper()
	if ctx.Err() != nil {
		r.t.Fatalf("finalization canceled: %v", ctx.Err())
	}
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) > 3*time.Second {
		r.t.Fatal("missing finalization deadline")
	}
	if r.finalDeadline.IsZero() {
		r.finalDeadline = deadline
	} else if !deadline.Equal(r.finalDeadline) {
		r.t.Fatal("finalization budget restarted")
	}
}

func (r *confirmRig) StartRun(
	ctx context.Context, o service.AgentAuditOwner,
) (*model.AgentRun, error) {
	r.checkOwner(o)
	if err := r.note(ctx, "start_run", 36*time.Second); err != nil {
		return nil, err
	}
	copy := r.run
	switch r.badRun {
	case "nil":
		return nil, nil
	case "owner":
		copy.UserID++
	case "status":
		copy.ExecutionStatus = "completed"
	case "id":
		copy.ID = "invalid"
	}
	return &copy, nil
}

func (r *confirmRig) StartCall(
	ctx context.Context,
	o service.AgentAuditOwner,
	id string,
	s service.AgentCallStart,
) (*model.ToolCall, error) {
	r.checkOwner(o)
	if id != r.run.ID || s.Binding == nil || *s.Binding != r.binding {
		r.t.Fatal("StartCall association")
	}
	confirmOK(r.t, service.ValidateAgentCallStart(o, s))
	raw, err := json.Marshal(r.request.Input)
	confirmOK(r.t, err)
	if s.ArgumentBytes != uint32(len(raw)) || s.ModelCallID != "" {
		r.t.Fatal("StartCall arguments")
	}
	if err := r.note(ctx, "start_call", 36*time.Second); err != nil {
		return nil, err
	}
	r.call = &model.ToolCall{
		ID: confirmCallID, RunID: id, Sequence: 1,
		ToolName:       tools.CreateAppealTicketName,
		Status:         "started",
		ErrorCode:      "none",
		BusinessEffect: "none",
		Confirmed:      true,
		OperationKey:   s.Binding.IdempotencyKey,
		RequestHash:    s.Binding.RequestHash,
		PolicyDigest:   s.Binding.PolicyDigest,
	}
	copy := *r.call
	switch r.badCall {
	case "nil":
		return nil, nil
	case "sequence":
		copy.Sequence = 2
	case "binding":
		copy.RequestHash = strings.Repeat("c", 64)
	case "confirmed":
		copy.Confirmed = false
	case "authorized":
		copy.WriteAuthorized = true
	}
	return &copy, nil
}

func (r *confirmRig) AuthorizeCreate(
	ctx context.Context,
	o service.AgentAuditOwner,
	runID, callID string,
	b service.AppealConfirmationBinding,
) error {
	r.checkOwner(o)
	if runID != r.run.ID || callID != confirmCallID ||
		b != r.binding || r.call == nil {
		r.t.Fatal("permit association")
	}
	if err := r.note(ctx, "authorize", 5*time.Second); err != nil {
		return err
	}
	r.call.WriteAuthorized = true
	r.call.BusinessEffect = "unknown"
	return nil
}

func (r *confirmRig) FinishCall(
	ctx context.Context,
	o service.AgentAuditOwner,
	runID, callID string,
	f service.AgentCallFinish,
) error {
	r.checkOwner(o)
	r.checkFinal(ctx)
	if runID != r.run.ID || callID != confirmCallID {
		r.t.Fatal("FinishCall association")
	}
	confirmOK(r.t, service.ValidateAgentCallFinish(f))
	copy := f
	r.finishCall = &copy
	if err := r.note(ctx, "finish_call", 3*time.Second); err != nil {
		return err
	}
	r.call.Status = f.Status
	r.call.ErrorCode = f.ErrorCode
	r.call.BusinessEffect = f.BusinessEffect
	if f.Ticket != nil {
		ticket := *f.Ticket
		r.call.TicketID = &ticket.TicketID
		r.call.TicketCreatedAt = &ticket.CreatedAt
		r.call.TicketStatus = ticket.InitialStatus
		r.call.Replayed = ticket.Replayed
	}
	return nil
}

func (r *confirmRig) FinishRun(
	ctx context.Context,
	o service.AgentAuditOwner,
	id string,
	f service.AgentRunFinish,
) error {
	r.checkOwner(o)
	r.checkFinal(ctx)
	if id != r.run.ID || f.ModelCalls != 0 {
		r.t.Fatal("FinishRun association/model calls")
	}
	confirmOK(r.t, service.ValidateAgentRunFinish(f))
	copy := f
	r.finishRun = &copy
	if err := r.note(ctx, "finish_run", 3*time.Second); err != nil {
		return err
	}
	r.run.ExecutionStatus = f.Status
	r.run.Decision = f.Decision
	r.run.ReasonCode = f.ReasonCode
	if r.call != nil {
		r.run.BusinessEffect = r.call.BusinessEffect
	}
	return nil
}

func (r *confirmRig) ReadOwned(
	ctx context.Context,
	o service.AgentAuditOwner,
	id string,
) (*service.AgentAuditSnapshot, error) {
	if err := r.note(ctx, "read_owned", 8*time.Second); err != nil {
		return nil, err
	}
	if o.UserID != 7 || o.ConversationID != 9 || id != r.run.ID {
		return nil, service.ErrAgentRunNotFound
	}
	if r.readErr != nil {
		return nil, r.readErr
	}
	s := &service.AgentAuditSnapshot{
		Run: r.run, Calls: []model.ToolCall{},
	}
	if r.call != nil {
		s.Calls = append(s.Calls, *r.call)
	}
	return s, nil
}

func (r *confirmRig) CheckConversationOwnership(
	ctx context.Context, u, c uint64,
) error {
	if u != 7 || c != 9 {
		return service.ErrConversationNotFound
	}
	return ctx.Err()
}

func (r *confirmRig) CheckAppealEvidence(
	context.Context, []model.AppealRuleEvidence, time.Time,
) (bool, error) {
	r.t.Fatal("no-policy flow must not invent rule evidence")
	return false, nil
}

func (r *confirmRig) GetByOperation(
	ctx context.Context, u uint64, key string,
) (*model.AppealTicket, error) {
	r.operationReads++
	if u != 7 || key != r.binding.IdempotencyKey {
		r.t.Fatal("operation lookup identity")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.operationErr != nil {
		return nil, r.operationErr
	}
	if r.ticket == nil {
		return nil, service.ErrAppealTicketNotFound
	}
	copy := *r.ticket
	return &copy, nil
}

func (r *confirmRig) GetByID(
	context.Context, uint64, uint64,
) (*model.AppealTicket, error) {
	r.t.Fatal("confirmed execution queried ticket by ID")
	return nil, nil
}

func (r *confirmRig) CreateWithRecord(
	ctx context.Context,
	c service.AppealCreateCommand,
	check service.AppealTransactionCheck,
) (*service.AppealCreateResult, error) {
	r.txCalls++
	r.trace = append(r.trace, "transaction")
	if r.call == nil || !r.call.WriteAuthorized {
		r.t.Fatal("transaction before durable permit")
	}
	if c.UserID != 7 || c.ConversationID != 9 ||
		c.IdempotencyKey != r.binding.IdempotencyKey ||
		c.RequestHash != r.binding.RequestHash ||
		c.ExpectedPolicyDigest != r.binding.PolicyDigest ||
		c.Input != r.request.Input {
		r.t.Fatal("transaction command binding")
	}
	if r.failAt == "transaction" {
		return nil, service.ErrAppealOutcomeUnknown
	}
	if r.expireInTransaction {
		r.now = r.now.Add(10 * time.Minute)
	}
	snapshot, err := check(ctx, r, r.now)
	if err != nil {
		return nil, err
	}
	status, err := service.AppealInitialStatus(snapshot, c.Input.IntakeMode)
	if err != nil {
		return nil, err
	}
	r.ticket = &model.AppealTicket{
		ID: 51, UserID: c.UserID, ConversationID: c.ConversationID,
		SubjectType: c.Input.SubjectType, SubjectRef: c.Input.SubjectRef,
		Reason: c.Input.Reason, IntakeMode: c.Input.IntakeMode,
		Status: status, IdempotencyKey: c.IdempotencyKey,
		RequestHash: c.RequestHash, EligibilitySnapshot: snapshot,
		CreatedAt: r.now, UpdatedAt: r.now,
	}
	if r.cancelAfterCommit != nil {
		r.cancelAfterCommit()
	}
	return &service.AppealCreateResult{
		TicketID: 51, InitialStatus: status, CreatedAt: r.now,
	}, nil
}

func (r *confirmRig) sink(ctx context.Context, e service.AgentEvent) error {
	name := e.Kind
	if err := r.note(ctx, name, 36*time.Second); err != nil {
		return err
	}
	if e.RunID != r.run.ID {
		r.t.Fatal("event run ID")
	}
	if name == "tool_start" || name == "tool_end" {
		if e.CallID != confirmCallID || e.Sequence != 1 ||
			e.ToolName != tools.CreateAppealTicketName {
			r.t.Fatal("event call association")
		}
	}
	r.events = append(r.events, e)
	if name == "tool_end" {
		r.checkFinal(ctx)
	}
	return nil
}

func (r *confirmRig) wantTrace(want string) {
	r.t.Helper()
	if strings.Join(r.trace, ",") != want {
		r.t.Fatalf("trace=%s; want=%s", strings.Join(r.trace, ","), want)
	}
}

func (r *confirmRig) assertPrivate(out *service.AgentOutcome) {
	r.t.Helper()
	b, err := json.Marshal(struct {
		Outcome *service.AgentOutcome
		Events  []service.AgentEvent
	}{out, r.events})
	confirmOK(r.t, err)
	for _, secret := range []string{
		r.request.Input.Reason,
		r.request.Input.SubjectRef,
		r.request.Confirmation.Token,
		r.binding.IdempotencyKey,
		r.binding.RequestHash,
		r.binding.PolicyDigest,
	} {
		if strings.Contains(string(b), secret) {
			r.t.Fatal("public output contains internal input")
		}
	}
}
