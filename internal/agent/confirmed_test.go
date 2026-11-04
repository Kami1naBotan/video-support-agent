package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"video-support-agent/internal/model"
	"video-support-agent/internal/service"
)

const confirmBeforeWrite = "start_run,agent_start,start_call,tool_start,authorize"
const confirmFullTrace = confirmBeforeWrite + ",transaction,finish_call,tool_end,finish_run"

func TestConfirmedExecution(t *testing.T) {
	cases := []struct {
		name, fail, badRun, badCall, code, effect, trace string
		pending                                          bool
		transactions                                     int
	}{
		{"success", "", "", "", "", "committed", confirmFullTrace, false, 1},
		{"start_run", "start_run", "", "", "audit_unavailable", "none", "start_run", true, 0},
		{"start_call", "start_call", "", "", "audit_unavailable", "none", "start_run,agent_start,start_call", true, 0},
		{"nil_run", "", "nil", "", "invalid_result", "none", "start_run", true, 0},
		{"run_owner", "", "owner", "", "invalid_result", "none", "start_run", true, 0},
		{"run_status", "", "status", "", "invalid_result", "none", "start_run", true, 0},
		{"run_id", "", "id", "", "invalid_result", "none", "start_run", true, 0},
		{"nil_call", "", "", "nil", "invalid_result", "none", "start_run,agent_start,start_call", true, 0},
		{"call_sequence", "", "", "sequence", "invalid_result", "none", "start_run,agent_start,start_call", true, 0},
		{"call_binding", "", "", "binding", "invalid_result", "none", "start_run,agent_start,start_call", true, 0},
		{"call_confirmation", "", "", "confirmed", "invalid_result", "none", "start_run,agent_start,start_call", true, 0},
		{"call_authorized", "", "", "authorized", "invalid_result", "none", "start_run,agent_start,start_call", true, 0},
		{"agent_start", "agent_start", "", "", "event_failed", "none", "start_run,agent_start,finish_run", false, 0},
		{"tool_start", "tool_start", "", "", "event_failed", "none", "start_run,agent_start,start_call,tool_start,finish_call,finish_run", false, 0},
		{"authorize", "authorize", "", "", "audit_unavailable", "none", confirmBeforeWrite + ",tool_end", true, 0},
		{"transaction_unknown", "transaction", "", "", "outcome_unknown", "unknown", confirmFullTrace, false, 1},
		{"finish_call", "finish_call", "", "", "audit_unavailable", "committed", confirmBeforeWrite + ",transaction,finish_call,tool_end", true, 1},
		{"tool_end", "tool_end", "", "", "event_failed", "committed", confirmFullTrace, false, 1},
		{"finish_run", "finish_run", "", "", "audit_unavailable", "committed", confirmFullTrace, true, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newConfirmRig(t)
			r.failAt, r.badRun, r.badCall = tc.fail, tc.badRun, tc.badCall
			out, err := r.executor.Confirm(context.Background(), r.request, r.sink)
			confirmCode(t, err, tc.code)
			if out == nil || out.BusinessEffect != tc.effect ||
				out.AuditPending != tc.pending ||
				out.ReasonCode != tc.code && tc.code != "" {
				t.Fatalf("unexpected outcome: %+v", out)
			}
			if out.MessageSaveStatus != "not_attempted" ||
				out.Proposal != nil || len(out.Citations) != 0 {
				t.Fatal("confirmed execution changed unrelated output")
			}
			r.wantTrace(tc.trace)
			if r.txCalls != tc.transactions {
				t.Fatalf("transactions=%d", r.txCalls)
			}
			if tc.effect == "committed" {
				if out.Ticket == nil || out.Ticket.TicketID != 51 ||
					out.Decision != "ticket_created" ||
					out.Ticket.InitialStatus != model.AppealTicketPendingReview {
					t.Fatal("committed receipt lost")
				}
				if r.finishCall == nil || r.finishCall.Status != "succeeded" ||
					r.finishCall.ErrorCode != "none" || r.finishCall.Ticket == nil {
					t.Fatal("committed tool recorded as failed")
				}
			} else if out.Ticket != nil {
				t.Fatal("unconfirmed ticket returned")
			}
			if tc.effect == "unknown" && out.ExecutionStatus != "unknown" {
				t.Fatal("unknown outcome collapsed")
			}
			if tc.code == "" &&
				(out.ExecutionStatus != "completed" || out.ReasonCode != "none") {
				t.Fatal("success status")
			}
			if r.finishRun != nil && r.finishRun.Decision != out.Decision {
				t.Fatal("run decision differs from result")
			}
			r.assertPrivate(out)
		})
	}
}

func TestConfirmedRealSignature(t *testing.T) {
	t.Run("two_verifications", func(t *testing.T) {
		r := newConfirmRig(t)
		out, err := r.executor.Confirm(context.Background(), r.request, r.sink)
		confirmCode(t, err, "")
		if r.signer.verifies != 2 || r.txCalls != 1 || out.BusinessEffect != "committed" {
			t.Fatal("real signer/transaction callback not used")
		}
		if len(r.events) != 3 || r.events[2].TicketID != "51" ||
			r.events[2].Status != "succeeded" {
			t.Fatal("success events")
		}
	})
	t.Run("expired_new", func(t *testing.T) {
		r := newConfirmRig(t)
		r.now = r.now.Add(10 * time.Minute)
		out, err := r.executor.Confirm(context.Background(), r.request, r.sink)
		confirmCode(t, err, "confirmation_required")
		if r.signer.verifies != 1 || r.txCalls != 0 ||
			r.ticket != nil || out.BusinessEffect != "none" {
			t.Fatal("expired new operation wrote")
		}
		r.wantTrace(confirmBeforeWrite + ",finish_call,tool_end,finish_run")
		if r.finishCall.Status != "rejected" {
			t.Fatal("expiry not recorded as rejection")
		}
	})
	t.Run("expires_in_transaction", func(t *testing.T) {
		r := newConfirmRig(t)
		r.expireInTransaction = true
		out, err := r.executor.Confirm(context.Background(), r.request, r.sink)
		confirmCode(t, err, "confirmation_required")
		if r.signer.verifies != 2 || r.txCalls != 1 ||
			r.ticket != nil || out.BusinessEffect != "none" {
			t.Fatal("transaction revalidation missed expiry")
		}
	})
	t.Run("expired_successful_replay", func(t *testing.T) {
		r := newConfirmRig(t)
		first, err := r.executor.Confirm(context.Background(), r.request, r.sink)
		confirmCode(t, err, "")
		r.now = r.now.Add(11 * time.Minute)
		r.resetAudit()
		r.run.ID = "00000000-0000-4000-8000-000000000003"
		out, err := r.executor.Confirm(context.Background(), r.request, r.sink)
		confirmCode(t, err, "")
		if out.Ticket == nil || !out.Ticket.Replayed ||
			out.Ticket.TicketID != first.Ticket.TicketID ||
			r.txCalls != 1 || r.signer.verifies != 2 {
			t.Fatal("successful replay revalidated expiry or recreated ticket")
		}
		r.wantTrace(confirmBeforeWrite + ",finish_call,tool_end,finish_run")
		if !r.events[len(r.events)-1].Replayed {
			t.Fatal("replay event lost marker")
		}
	})
}

func TestConfirmedCancellation(t *testing.T) {
	t.Run("before_start", func(t *testing.T) {
		r := newConfirmRig(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		out, err := r.executor.Confirm(ctx, r.request, r.sink)
		confirmCode(t, err, "cancelled")
		r.wantTrace("")
		if out.BusinessEffect != "none" || r.txCalls != 0 {
			t.Fatal("canceled request wrote")
		}
	})
	t.Run("after_commit", func(t *testing.T) {
		r := newConfirmRig(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		r.cancelAfterCommit = cancel
		out, err := r.executor.Confirm(ctx, r.request, r.sink)
		confirmCode(t, err, "cancelled")
		if out.Ticket == nil || out.BusinessEffect != "committed" ||
			out.Decision != "ticket_created" || out.AuditPending ||
			out.ExecutionStatus != "cancelled" {
			t.Fatal("cancellation lost committed fact")
		}
		r.wantTrace(confirmBeforeWrite + ",transaction,finish_call,finish_run")
		if r.finishCall.Status != "succeeded" || r.finishRun.Status != "cancelled" {
			t.Fatal("business/execution status conflated")
		}
		r.assertPrivate(out)
	})
	t.Run("insufficient_budget", func(t *testing.T) {
		r := newConfirmRig(t)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, err := r.executor.Confirm(ctx, r.request, r.sink)
		confirmCode(t, err, "timed_out")
		r.wantTrace("")
	})
	t.Run("nil_sink", func(t *testing.T) {
		r := newConfirmRig(t)
		out, err := r.executor.Confirm(context.Background(), r.request, nil)
		confirmCode(t, err, "")
		if out.BusinessEffect != "committed" || out.AuditPending {
			t.Fatal("nil sink broke execution")
		}
		r.wantTrace("start_run,start_call,authorize,transaction,finish_call,finish_run")
	})
}

func TestConfirmedInput(t *testing.T) {
	cases := []struct {
		name, code string
		edit       func(*service.AgentConfirmedRequest)
	}{
		{"user", "unauthorized", func(q *service.AgentConfirmedRequest) { q.UserID = 0 }},
		{"conversation", "invalid_argument", func(q *service.AgentConfirmedRequest) { q.ConversationID = 0 }},
		{"missing_facts", "needs_more_info", func(q *service.AgentConfirmedRequest) { q.Input.Reason = "" }},
		{"key", "invalid_argument", func(q *service.AgentConfirmedRequest) { q.Confirmation.IdempotencyKey = "" }},
		{"digest", "invalid_argument", func(q *service.AgentConfirmedRequest) { q.Confirmation.PolicyDigest = "" }},
		{"token", "confirmation_required", func(q *service.AgentConfirmedRequest) { q.Confirmation.Token = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newConfirmRig(t)
			request := r.request
			tc.edit(&request)
			out, err := r.executor.Confirm(context.Background(), request, r.sink)
			confirmCode(t, err, tc.code)
			r.wantTrace("")
			if out.RunID != "" || out.BusinessEffect != "none" || r.txCalls != 0 {
				t.Fatal("invalid request started execution")
			}
		})
	}
	t.Run("nil_executor", func(t *testing.T) {
		var executor *ConfirmedExecutor
		_, err := executor.Confirm(
			context.Background(), service.AgentConfirmedRequest{}, nil,
		)
		confirmCode(t, err, "configuration_error")
	})
}

func TestConfirmedPermit(t *testing.T) {
	t.Run("single_use", func(t *testing.T) {
		r := newConfirmRig(t)
		r.call = &model.ToolCall{ID: confirmCallID}
		p := &createPermit{
			store: r,
			owner: service.AgentAuditOwner{UserID: 7, ConversationID: 9},
			runID: r.run.ID, callID: confirmCallID, expected: r.binding,
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		confirmOK(t, p.BeforeCreate(ctx, r.binding))
		if err := p.BeforeCreate(ctx, r.binding); !errors.Is(err, service.ErrAgentAuditConflict) {
			t.Fatal(err)
		}
		r.wantTrace("authorize")
		if !p.failed {
			t.Fatal("reused permit was not marked failed")
		}
	})
	t.Run("binding_mismatch", func(t *testing.T) {
		r := newConfirmRig(t)
		p := &createPermit{
			store: r, runID: r.run.ID,
			callID: confirmCallID, expected: r.binding,
		}
		changed := r.binding
		changed.RequestHash = ""
		if err := p.BeforeCreate(context.Background(), changed); !errors.Is(err, service.ErrAgentAuditConflict) {
			t.Fatal(err)
		}
		r.wantTrace("")
		if !p.failed {
			t.Fatal("invalid permit was not marked failed")
		}
	})
}

func TestConfirmedRecovery(t *testing.T) {
	cases := []struct {
		name, effect, code string
		edit               func(*confirmRig)
	}{
		{"committed", "committed", "none", func(r *confirmRig) {}},
		{"observed_commit", "committed", "none", func(r *confirmRig) {
			r.run.BusinessEffect = "unknown"
			r.call.BusinessEffect = "unknown"
			r.call.Status = "started"
			r.call.TicketID = nil
			r.call.TicketCreatedAt = nil
		}},
		{"pending_absent", "unknown", "outcome_unknown", func(r *confirmRig) {
			r.ticket = nil
			r.run.BusinessEffect = "unknown"
			r.call.BusinessEffect = "unknown"
			r.call.Status = "started"
			r.call.TicketID = nil
			r.call.TicketCreatedAt = nil
		}},
		{"committed_read_error", "committed", "outcome_unknown", func(r *confirmRig) {
			r.operationErr = errors.New("private-storage-error")
		}},
		{"conflict", "committed", "idempotency_conflict", func(r *confirmRig) {
			r.operationErr = service.ErrAppealIdempotencyConflict
		}},
		{"not_authorized", "none", "none", func(r *confirmRig) {
			r.run.BusinessEffect = "none"
			r.call.WriteAuthorized = false
			r.call.BusinessEffect = "none"
			r.call.TicketID = nil
			r.call.TicketCreatedAt = nil
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newConfirmRig(t)
			_, err := r.executor.Confirm(context.Background(), r.request, r.sink)
			confirmCode(t, err, "")
			tc.edit(r)
			storedEffect := r.run.BusinessEffect
			storedCall := r.call.BusinessEffect
			r.trace = nil
			readsBefore := r.operationReads

			got, err := r.executor.Recover(
				context.Background(),
				service.AgentAuditOwner{UserID: 7, ConversationID: 9},
				r.run.ID,
			)
			confirmCode(t, err, "")
			r.wantTrace("read_owned")
			if got == nil || got.BusinessEffect != tc.effect || got.RecoveryCode != tc.code {
				t.Fatalf("observation=%+v", got)
			}
			if (got.Ticket != nil) != (tc.effect == "committed") {
				t.Fatal("recovery receipt")
			}
			if r.txCalls != 1 || r.run.BusinessEffect != storedEffect ||
				r.call.BusinessEffect != storedCall {
				t.Fatal("read-only recovery mutated state")
			}
			expectedReads := 1
			if tc.name == "not_authorized" {
				expectedReads = 0
			}
			if r.operationReads-readsBefore != expectedReads {
				t.Fatal("unexpected recovery lookup count")
			}
		})
	}
	t.Run("foreign_owner", func(t *testing.T) {
		r := newConfirmRig(t)
		got, err := r.executor.Recover(
			context.Background(),
			service.AgentAuditOwner{UserID: 8, ConversationID: 9},
			r.run.ID,
		)
		confirmCode(t, err, "not_found")
		if got != nil {
			t.Fatal("foreign snapshot returned")
		}
		r.wantTrace("read_owned")
	})
	t.Run("audit_read_failure", func(t *testing.T) {
		r := newConfirmRig(t)
		r.readErr = service.ErrAgentAuditUnavailable
		got, err := r.executor.Recover(
			context.Background(),
			service.AgentAuditOwner{UserID: 7, ConversationID: 9},
			r.run.ID,
		)
		confirmCode(t, err, "audit_unavailable")
		if got != nil {
			t.Fatal("failed audit read returned observation")
		}
	})
	t.Run("invalid_run", func(t *testing.T) {
		r := newConfirmRig(t)
		_, err := r.executor.Recover(
			context.Background(),
			service.AgentAuditOwner{UserID: 7, ConversationID: 9},
			"invalid",
		)
		confirmCode(t, err, "invalid_argument")
		r.wantTrace("")
	})
}

func TestConfirmedBudget(t *testing.T) {
	t.Run("shared_cancel_and_release", func(t *testing.T) {
		parent, cancel := context.WithCancel(context.Background())
		defer cancel()
		b := newRunBudget(parent)
		defer b.close()
		total, _ := b.total.Deadline()
		work, _ := b.work.Deadline()
		if total.Sub(work) != 4*time.Second || time.Until(total) > 40*time.Second {
			t.Fatal("work/total reserve")
		}
		cancel()
		if b.work.Err() != context.Canceled {
			t.Fatal("work ignored cancellation")
		}
		first, err := service.AppealFinalizationContext(b.total)
		confirmOK(t, err)
		second, err := service.AppealFinalizationContext(b.work)
		confirmOK(t, err)
		one, _ := first.Deadline()
		two, _ := second.Deadline()
		if first.Err() != nil || !one.Equal(two) || time.Until(one) > 3*time.Second {
			t.Fatal("shared finalization budget")
		}
		b.close()
		if first.Err() != context.Canceled {
			t.Fatal("finalization was not released")
		}
	})
	t.Run("parent_deadline", func(t *testing.T) {
		parent, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		b := newRunBudget(parent)
		defer b.close()
		expected, _ := parent.Deadline()
		total, _ := b.total.Deadline()
		work, _ := b.work.Deadline()
		if !total.Equal(expected) || !work.Equal(expected.Add(-4*time.Second)) {
			t.Fatal("parent deadline not preserved")
		}
	})
}

func TestConfirmedConfiguration(t *testing.T) {
	for _, name := range []string{
		"conversations", "retriever", "knowledge", "appeals", "audit", "recovery",
	} {
		t.Run(name, func(t *testing.T) {
			r := newConfirmRig(t)
			deps := r.executor.deps
			var audit service.AgentAuditStore = r
			var recovery service.AgentAppealRecovery = r.svc
			switch name {
			case "conversations":
				deps.Conversations = nil
			case "retriever":
				deps.Retriever = nil
			case "knowledge":
				deps.Knowledge = nil
			case "appeals":
				var missing *service.AppealTicketService
				deps.Appeals = missing
			case "audit":
				var missing *confirmRig
				audit = missing
			case "recovery":
				var missing *service.AppealTicketService
				recovery = missing
			}
			got, err := NewConfirmedExecutor(deps, audit, recovery)
			if got != nil || !errors.Is(err, service.ErrAgentAuditConfiguration) {
				t.Fatalf("constructor result=%v error=%v", got, err)
			}
		})
	}
}
