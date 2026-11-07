package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"video-support-agent/internal/model"
)

// 仅测试用：严格比对已签发绑定和时间，不提供密码学证明。
type Confirm struct {
	binding             AppealConfirmationBinding
	start, end, now     time.Time
	issued, verified    int
	issueErr, verifyErr error
	token               string
}

func (c *Confirm) Issue(
	_ context.Context,
	b AppealConfirmationBinding,
	start, end time.Time,
) (string, error) {
	c.issued++
	c.binding, c.start, c.end = b, start, end
	return c.token, c.issueErr
}

func (c *Confirm) Verify(
	ctx context.Context, token string, b AppealConfirmationBinding,
) error {
	c.verified++
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.verifyErr != nil {
		return c.verifyErr
	}
	if token != c.token || b != c.binding ||
		c.now.Before(c.start) || !c.now.Before(c.end) {
		return ErrAppealConfirmationRequired
	}
	return nil
}

type Store struct {
	t                               *testing.T
	saved                           *model.AppealTicket
	readErr                         error
	creates, writes, reads, queries int
	at                              time.Time
	checker                         AppealEvidenceChecker
	before                          func()
	after                           func()
	output                          func(*AppealCreateResult) (*AppealCreateResult, error)
}

func (s *Store) CheckConversationOwnership(
	_ context.Context, user, conversation uint64,
) error {
	if (user == 1 && (conversation == 7 || conversation == 8)) ||
		(user == 2 && conversation == 9) {
		return nil
	}
	return ErrConversationNotFound
}

func (s *Store) CheckAppealEvidence(
	context.Context, []model.AppealRuleEvidence, time.Time,
) (bool, error) {
	s.t.Fatal("transaction callback used outer store instead of transaction checker")
	return false, ErrAppealStorage
}

func (s *Store) GetByOperation(
	_ context.Context, user uint64, key string,
) (*model.AppealTicket, error) {
	s.reads++
	if s.readErr != nil {
		return nil, s.readErr
	}
	if s.saved == nil || s.saved.UserID != user ||
		s.saved.IdempotencyKey != key {
		return nil, ErrAppealTicketNotFound
	}
	copy := *s.saved
	return &copy, nil
}

func (s *Store) GetByID(
	_ context.Context, user, id uint64,
) (*model.AppealTicket, error) {
	s.queries++
	if s.readErr != nil {
		return nil, s.readErr
	}
	if s.saved == nil || s.saved.UserID != user || s.saved.ID != id {
		return nil, ErrAppealTicketNotFound
	}
	copy := *s.saved
	return &copy, nil
}

func (s *Store) CreateWithRecord(
	ctx context.Context,
	command AppealCreateCommand,
	check AppealTransactionCheck,
) (*AppealCreateResult, error) {
	s.creates++
	if s.before != nil {
		s.before()
	}
	snapshot, err := check(ctx, s.checker, s.at)
	if err != nil {
		return nil, err
	}
	if !snapshot.CheckedAt.Equal(s.at) {
		s.t.Fatal("transaction time not used")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	status, err := AppealInitialStatus(snapshot, command.Input.IntakeMode)
	if err != nil {
		return nil, err
	}
	s.saved = &model.AppealTicket{
		ID: 42, UserID: command.UserID,
		ConversationID:      command.ConversationID,
		SubjectType:         command.Input.SubjectType,
		SubjectRef:          command.Input.SubjectRef,
		Reason:              command.Input.Reason,
		IntakeMode:          command.Input.IntakeMode,
		Status:              status,
		IdempotencyKey:      command.IdempotencyKey,
		RequestHash:         command.RequestHash,
		EligibilitySnapshot: snapshot,
		CreatedAt:           s.at, UpdatedAt: s.at,
	}
	s.writes++
	result := &AppealCreateResult{
		TicketID: 42, InitialStatus: status, CreatedAt: s.at,
	}
	if s.after != nil {
		s.after()
	}
	if s.output != nil {
		return s.output(result)
	}
	return result, nil
}

func ServiceFixture(t *testing.T) (
	*AppealTicketService, *Store, *Confirm,
) {
	t.Helper()
	store := &Store{
		t: t, at: DomainAt.Add(time.Second),
	}
	confirm := &Confirm{
		now: DomainAt, token: "TEST_ONLY_TOKEN",
	}
	eligibility, err := NewAppealEligibilityService(NoAppealPolicies{})
	if err != nil {
		t.Fatal(err)
	}
	svc, err := NewAppealTicketServiceWithClock(
		store, eligibility, confirm,
		func() time.Time { return DomainAt },
	)
	if err != nil {
		t.Fatal(err)
	}
	return svc, store, confirm
}

func Prepare(t *testing.T, svc *AppealTicketService) *PreparedAppeal {
	t.Helper()
	p, err := svc.Prepare(context.Background(), 1, 7, DomainInput())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func Confirmation(p *PreparedAppeal) AppealConfirmationInput {
	return AppealConfirmationInput{
		IdempotencyKey: p.Binding.IdempotencyKey,
		PolicyDigest:   p.Binding.PolicyDigest,
		Token:          p.Token,
	}
}

func TestFlowPrepare(t *testing.T) {
	t.Run("binding_no_write", func(t *testing.T) {
		svc, store, confirm := ServiceFixture(t)
		p := Prepare(t, svc)
		hash, err := AppealRequestHash(7, DomainInput())
		if err != nil {
			t.Fatal(err)
		}
		digest, err := AppealPolicyDigest(p.Eligibility)
		if err != nil {
			t.Fatal(err)
		}
		if p.Binding.UserID != 1 || p.Binding.ConversationID != 7 ||
			p.Binding.RequestHash != hash || p.Binding.PolicyDigest != digest ||
			confirm.binding != p.Binding ||
			ValidateAppealOperationKey(p.Binding.IdempotencyKey) != nil ||
			p.InitialStatus != model.AppealTicketPendingReview ||
			p.Input != DomainInput() ||
			!p.IssuedAt.Equal(DomainAt) ||
			!p.ExpiresAt.Equal(DomainAt.Add(10*time.Minute)) ||
			store.creates != 0 || store.writes != 0 ||
			confirm.issued != 1 || confirm.verified != 0 {
			t.Fatalf("invalid proposal or side effect: %+v", p)
		}
		second := Prepare(t, svc)
		if second.Binding.IdempotencyKey == p.Binding.IdempotencyKey {
			t.Fatal("operation key reused")
		}
		for _, value := range []any{p, Confirmation(p)} {
			data, err := json.Marshal(value)
			if err != nil || string(data) != "{}" {
				t.Fatalf("private proposal leaked: %s %v", data, err)
			}
		}
	})

	for _, name := range []string{
		"nil_confirmation", "issue_error", "empty_token",
		"token_whitespace", "wrong_mode", "missing_fields",
	} {
		t.Run(name, func(t *testing.T) {
			svc, store, confirm := ServiceFixture(t)
			input := DomainInput()
			want := ErrAppealConfiguration
			switch name {
			case "nil_confirmation":
				svc.confirmations = nil
			case "issue_error":
				confirm.issueErr = ErrAppealStorage
				want = ErrAppealStorage
			case "empty_token":
				confirm.token = ""
			case "token_whitespace":
				confirm.token = "bad token"
			case "wrong_mode":
				input.IntakeMode = model.AppealIntakeStandard
				want = ErrAppealPreconditionChanged
			case "missing_fields":
				input.Reason = ""
				want = ErrAppealNeedsMoreInfo
			}
			p, err := svc.Prepare(context.Background(), 1, 7, input)
			if p != nil || !errors.Is(err, want) || store.creates != 0 {
				t.Fatalf("proposal=%v error=%v", p, err)
			}
		})
	}
}

func TestFlowCreate(t *testing.T) {
	for _, name := range []string{
		"success", "wrong_key", "changed_payload", "changed_mode",
		"wrong_digest", "wrong_user", "wrong_conversation", "empty_token",
		"expired", "nil_confirmation", "verify_error",
		"expires_in_transaction", "policy_changed", "transaction_checker",
		"commit_then_cancel", "nil_result", "bad_result",
		"unknown_storage", "canceled_before",
	} {
		t.Run(name, func(t *testing.T) {
			svc, store, confirm := ServiceFixture(t)
			p := Prepare(t, svc)
			input, confirmation := p.Input, Confirmation(p)
			user, conversation := uint64(1), uint64(7)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var want error
			wantCreates, wantWrites := 0, 0

			switch name {
			case "success":
				wantCreates, wantWrites = 1, 1
			case "wrong_key":
				confirmation.IdempotencyKey = "12345678-1234-4234-8234-123456789abc"
				want = ErrAppealConfirmationRequired
			case "changed_payload":
				input.Reason += "!"
				want = ErrAppealConfirmationRequired
			case "changed_mode":
				input.IntakeMode = model.AppealIntakeStandard
				want = ErrAppealConfirmationRequired
			case "wrong_digest":
				confirmation.PolicyDigest = strings.Repeat("f", 64)
				want = ErrAppealConfirmationRequired
			case "wrong_user":
				user, conversation = 2, 9
				want = ErrAppealConfirmationRequired
			case "wrong_conversation":
				conversation = 8
				want = ErrAppealConfirmationRequired
			case "empty_token":
				confirmation.Token = ""
				want = ErrAppealConfirmationRequired
			case "expired":
				confirm.now = confirm.end
				want = ErrAppealConfirmationRequired
			case "nil_confirmation":
				svc.confirmations = nil
				want = ErrAppealConfiguration
			case "verify_error":
				confirm.verifyErr = ErrAppealStorage
				want = ErrAppealStorage
			case "expires_in_transaction":
				store.before = func() { confirm.now = confirm.end }
				want, wantCreates = ErrAppealConfirmationRequired, 1
			case "policy_changed", "transaction_checker":
				policy := &DomainPolicy{
					def: AppealPolicyDefinition{
						Code: "TEST_ONLY", Revision: "v1",
						SubjectType:  model.AppealSubjectAccount,
						EvidenceRefs: DomainSnapshot().EvidenceRefs,
					},
					result: AppealPolicyEvaluation{
						Decision:   model.AppealEligibilityEligible,
						ReasonCode: "test_only_eligible",
					},
				}
				svc.eligibility, _ = NewAppealEligibilityService(
					DomainProvider{policy: policy},
				)
				store.checker = &DomainEvidence{current: true}
				if name == "policy_changed" {
					want, wantCreates = ErrAppealPreconditionChanged, 1
				} else {
					input.IntakeMode = model.AppealIntakeStandard
					snapshot, err := svc.eligibility.Evaluate(
						ctx, appealFactsFromInput(input),
						DomainAt, store.checker,
					)
					if err != nil {
						t.Fatal(err)
					}
					hash, err := AppealRequestHash(7, input)
					if err != nil {
						t.Fatal(err)
					}
					digest, err := AppealPolicyDigest(snapshot)
					if err != nil {
						t.Fatal(err)
					}
					confirm.binding.RequestHash = hash
					confirm.binding.PolicyDigest = digest
					confirmation.PolicyDigest = digest
					wantCreates, wantWrites = 1, 1
				}
			case "commit_then_cancel":
				store.after = cancel
				wantCreates, wantWrites = 1, 1
			case "nil_result":
				store.output = func(*AppealCreateResult) (*AppealCreateResult, error) {
					return nil, nil
				}
				want, wantCreates, wantWrites = ErrAppealOutcomeUnknown, 1, 1
			case "bad_result":
				store.output = func(r *AppealCreateResult) (*AppealCreateResult, error) {
					r.TicketID = 0
					return r, nil
				}
				want, wantCreates, wantWrites = ErrAppealOutcomeUnknown, 1, 1
			case "unknown_storage":
				store.output = func(*AppealCreateResult) (*AppealCreateResult, error) {
					return nil, ErrAppealOutcomeUnknown
				}
				want, wantCreates, wantWrites = ErrAppealOutcomeUnknown, 1, 1
			case "canceled_before":
				cancel()
				want = context.Canceled
			}

			result, err := svc.Create(
				ctx, user, conversation, input, confirmation,
			)
			if !errors.Is(err, want) ||
				store.creates != wantCreates || store.writes != wantWrites {
				t.Fatalf(
					"error=%v want=%v creates=%d/%d writes=%d/%d",
					err, want, store.creates, wantCreates, store.writes, wantWrites,
				)
			}
			if want != nil {
				if result != nil {
					t.Fatal("failure returned a success result")
				}
				return
			}
			if result == nil || result.TicketID != 42 ||
				result.Replayed || !result.CreatedAt.Equal(store.at) ||
				confirm.verified != 2 {
				t.Fatalf("result=%+v verified=%d", result, confirm.verified)
			}
			if name == "transaction_checker" {
				if result.InitialStatus != model.AppealTicketSubmitted ||
					store.checker.(*DomainEvidence).calls != 2 {
					t.Fatal("transaction checker/status mismatch")
				}
			}
		})
	}
}

func TestFlowReplayRecovery(t *testing.T) {
	for _, name := range []string{
		"expired_replay", "nil_replay", "conflict", "recover_found",
		"recover_absent", "recover_error", "recover_conflict", "corrupt_saved",
	} {
		t.Run(name, func(t *testing.T) {
			svc, store, confirm := ServiceFixture(t)
			p := Prepare(t, svc)
			first, err := svc.Create(
				context.Background(), 1, 7, p.Input, Confirmation(p),
			)
			if err != nil {
				t.Fatal(err)
			}
			verified := confirm.verified
			var want error
			input := p.Input
			hash := p.Binding.RequestHash

			switch name {
			case "expired_replay":
				confirm.now = confirm.end
			case "nil_replay":
				svc.confirmations = nil
			case "conflict":
				input.Reason += "!"
				want = ErrAppealIdempotencyConflict
			case "recover_absent":
				store.saved = nil
				want = ErrAppealOutcomeUnknown
			case "recover_error":
				store.readErr = ErrAppealStorage
				want = ErrAppealOutcomeUnknown
			case "recover_conflict":
				hash = strings.Repeat("a", 64)
				want = ErrAppealIdempotencyConflict
			case "corrupt_saved":
				store.saved.CreatedAt = time.Time{}
				want = ErrAppealStorage
			}

			var result *AppealCreateResult
			if strings.HasPrefix(name, "recover_") {
				result, err = svc.Recover(
					context.Background(), 1, 7,
					p.Binding.IdempotencyKey, hash,
				)
			} else {
				result, err = svc.Create(
					context.Background(), 1, 7, input, Confirmation(p),
				)
			}
			if !errors.Is(err, want) ||
				store.creates != 1 || store.writes != 1 ||
				confirm.verified != verified {
				t.Fatalf("error=%v want=%v creates=%d verified=%d",
					err, want, store.creates, confirm.verified)
			}
			if want == nil &&
				(result == nil || result.TicketID != first.TicketID ||
					!result.Replayed || result.InitialStatus != first.InitialStatus ||
					!result.CreatedAt.Equal(first.CreatedAt)) {
				t.Fatalf("replay=%+v", result)
			}
		})
	}
}

func TestFlowAccess(t *testing.T) {
	for _, name := range []string{
		"unauthenticated", "foreign_conversation", "query_own",
		"query_other", "query_absent", "query_invalid_id", "query_error",
	} {
		t.Run(name, func(t *testing.T) {
			svc, store, _ := ServiceFixture(t)
			p := Prepare(t, svc)
			if _, err := svc.Create(
				context.Background(), 1, 7, p.Input, Confirmation(p),
			); err != nil {
				t.Fatal(err)
			}

			if name == "unauthenticated" || name == "foreign_conversation" {
				user, conversation, want := uint64(0), uint64(7), ErrUnauthorized
				if name == "foreign_conversation" {
					user, conversation, want = 1, 9, ErrConversationNotFound
				}
				_, a := svc.CheckEligibility(
					context.Background(), user, conversation,
					appealFactsFromInput(p.Input),
				)
				_, b := svc.Prepare(
					context.Background(), user, conversation, p.Input,
				)
				_, c := svc.Create(
					context.Background(), user, conversation,
					p.Input, Confirmation(p),
				)
				_, d := svc.Recover(
					context.Background(), user, conversation,
					p.Binding.IdempotencyKey, p.Binding.RequestHash,
				)
				_, e := svc.Query(context.Background(), user, conversation, 42)
				for _, err := range []error{a, b, c, d, e} {
					if !errors.Is(err, want) {
						t.Fatalf("access error=%v want=%v", err, want)
					}
				}
				if store.creates != 1 || store.queries != 0 {
					t.Fatal("unauthorized call reached storage action")
				}
				return
			}

			user, conversation, id := uint64(1), uint64(8), uint64(42)
			var want error
			switch name {
			case "query_other":
				user, conversation = 2, 9
				want = ErrAppealTicketNotFound
			case "query_absent":
				id = 99
				want = ErrAppealTicketNotFound
			case "query_invalid_id":
				id = 0
				want = ErrInvalidArgument
			case "query_error":
				store.readErr = ErrAppealStorage
				want = ErrAppealStorage
			}
			view, err := svc.Query(context.Background(), user, conversation, id)
			if !errors.Is(err, want) {
				t.Fatalf("query=%v want=%v", err, want)
			}
			if want == nil {
				if view == nil || view.TicketID != 42 ||
					view.Status != model.AppealTicketPendingReview {
					t.Fatalf("view=%+v", view)
				}
				data, err := json.Marshal(view)
				if err != nil {
					t.Fatal(err)
				}
				for _, secret := range []string{
					"reason", "idempotency_key", "request_hash",
					"eligibility_snapshot", "token", "user_id",
				} {
					if strings.Contains(string(data), secret) {
						t.Fatalf("private field in view: %s", data)
					}
				}
			}
		})
	}
}

func TestFlowConfiguration(t *testing.T) {
	svc, store, _ := ServiceFixture(t)
	for _, tc := range []struct {
		name        string
		store       AppealTicketStore
		eligibility *AppealEligibilityService
		clock       func() time.Time
	}{
		{"store", nil, svc.eligibility, time.Now},
		{"eligibility", store, nil, time.Now},
		{"policy_provider", store, &AppealEligibilityService{}, time.Now},
		{"clock", store, svc.eligibility, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewAppealTicketServiceWithClock(
				tc.store, tc.eligibility, nil, tc.clock,
			); !errors.Is(err, ErrAppealConfiguration) {
				t.Fatalf("error=%v", err)
			}
		})
	}
	t.Run("read_without_confirmation", func(t *testing.T) {
		svc.confirmations = nil
		result, err := svc.CheckEligibility(
			context.Background(), 1, 7,
			appealFactsFromInput(DomainInput()),
		)
		if err != nil || result.Decision != model.AppealEligibilityManualReview {
			t.Fatalf("result=%+v err=%v", result, err)
		}
	})
}
