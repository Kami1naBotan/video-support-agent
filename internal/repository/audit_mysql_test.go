//go:build auditmysql

package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"video-support-agent/internal/database"
	"video-support-agent/internal/model"
	"video-support-agent/internal/service"

	driver "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func TestAgentAuditMySQL(t *testing.T) {
	db := auditDatabase(t)

	if !t.Run("fresh_migration", func(t *testing.T) {
		auditMust(t, database.Migrate(db))
		auditMust(t, database.Migrate(db))
		auditSchema(t, db)
	}) {
		return
	}

	t.Run("upgrade_preserves_data", func(t *testing.T) {
		old := auditDatabase(t)
		auditMust(t, old.AutoMigrate(
			&model.User{},
			&model.Conversation{},
			&model.Message{},
			&model.KnowledgeDocument{},
			&model.RuleCategory{},
			&model.RuleVersion{},
			&model.RuleArticle{},
			&model.KnowledgeChunk{},
			&model.RuleVerification{},
			&model.RuleOperation{},
			&model.AppealTicket{},
			&model.TicketRecord{},
		))

		if old.Migrator().HasTable(&model.AgentRun{}) ||
			old.Migrator().HasTable(&model.ToolCall{}) {
			t.Fatal("not a pre-audit schema")
		}

		f := auditSeed(t, old)
		proposal := f.prepare()
		ticket := f.create(proposal)

		auditMust(t, database.Migrate(old))
		auditMust(t, database.Migrate(old))
		auditSchema(t, old)

		view, err := f.svc.Query(
			f.ctx,
			f.owner.UserID,
			f.owner.ConversationID,
			ticket.TicketID,
		)
		auditMust(t, err)

		if view.TicketID != ticket.TicketID ||
			view.SubjectRef != proposal.Input.SubjectRef ||
			view.Status != model.AppealTicketPendingReview {
			t.Fatal("upgrade changed existing ticket")
		}

		var count int64
		auditMust(t, old.Model(&model.TicketRecord{}).
			Where("ticket_id = ?", ticket.TicketID).
			Count(&count).Error)
		if count != 1 {
			t.Fatal("upgrade lost business record")
		}
	})

	conflict := service.ErrAgentAuditConflict

	cases := []struct {
		name string
		run  func(*auditFixture)
	}{
		{"schema_constraints", func(f *auditFixture) {
			t := f.t
			run := f.run()
			call := f.call(run.ID, "search_rule", "Case")

			insert := func(
				sequence uint32,
				id *string,
				runID string,
				ticket *uint64,
			) error {
				copy := *call
				copy.ID = auditID(t)
				copy.Sequence = sequence
				copy.ModelCallID = id
				copy.RunID = runID
				copy.TicketID = ticket
				return f.db.Omit(clause.Associations).
					Create(&copy).Error
			}

			different := "different"
			auditNumber(t, insert(
				1, &different, run.ID, nil,
			), 1062)

			same := "Case"
			auditNumber(t, insert(
				2, &same, run.ID, nil,
			), 1062)

			lower := "case"
			auditMust(t, insert(2, &lower, run.ID, nil))
			auditMust(t, insert(3, nil, run.ID, nil))
			auditMust(t, insert(4, nil, run.ID, nil))

			auditNumber(t, insert(
				5, &different, auditID(t), nil,
			), 1452)

			absent := uint64(1 << 62)
			auditNumber(t, insert(
				5, &different, run.ID, &absent,
			), 1452)

			copy := *run
			copy.ID = auditID(t)
			copy.UserID = absent
			auditNumber(t, f.db.Omit(clause.Associations).
				Create(&copy).Error, 1452)
		}},

		{"lifecycle", func(f *auditFixture) {
			t := f.t
			run := f.run()
			first := f.call(run.ID, "search_rule", "first")
			auditMust(t, f.repo.FinishCall(
				f.ctx, f.owner, run.ID, first.ID, auditReadFinish(),
			))

			second := f.call(run.ID, "get_rule_detail", "second")
			f.now = f.now.Add(time.Second)
			auditMust(t, f.repo.FinishCall(
				f.ctx, f.owner, run.ID, second.ID, auditReadFinish(),
			))
			auditMust(t, f.repo.FinishRun(
				f.ctx, f.owner, run.ID, auditRunFinish(),
			))

			snapshot := f.read(run.ID)
			if len(snapshot.Calls) != 2 ||
				snapshot.Run.ToolCalls != 2 ||
				snapshot.Run.ModelCalls != 2 ||
				snapshot.Run.ExecutionStatus != "completed" ||
				snapshot.Run.BusinessEffect != "none" ||
				snapshot.Run.MessageSaveStatus != "not_attempted" ||
				snapshot.Run.FinishedAt == nil ||
				!snapshot.Run.StartedAt.Equal(auditAt) ||
				!snapshot.Run.FinishedAt.Equal(f.now) {
				t.Fatalf("run=%+v", snapshot.Run)
			}

			for i, call := range snapshot.Calls {
				if call.RunID != run.ID ||
					call.Sequence != uint32(i+1) ||
					call.Status != "succeeded" ||
					call.ArgumentBytes != 17 ||
					call.Confirmed ||
					call.WriteAuthorized ||
					call.FinishedAt == nil {
					t.Fatal("call roundtrip")
				}
			}
			if snapshot.Calls[1].DurationMS != 1000 {
				t.Fatal("duration precision")
			}

			auditWant(t, f.repo.FinishRun(
				f.ctx, f.owner, run.ID, auditRunFinish(),
			), conflict)

			_, err := f.repo.StartCall(
				f.ctx, f.owner, run.ID,
				service.AgentCallStart{
					ToolName: "search_rule", ModelCallID: "third",
					ArgumentBytes: 1,
				},
			)
			auditWant(t, err, conflict)
		}},

		{"ownership", func(f *auditFixture) {
			t := f.t
			run := f.run()
			proposal := f.prepare()
			call := f.confirmed(run, proposal)

			_, err := f.repo.ReadOwned(f.ctx, f.other, run.ID)
			auditWant(t, err, service.ErrAgentRunNotFound)

			_, err = f.repo.StartCall(
				f.ctx, f.other, run.ID,
				service.AgentCallStart{
					ToolName: "search_rule", ModelCallID: "x",
					ArgumentBytes: 1,
				},
			)
			auditWant(t, err, service.ErrAgentRunNotFound)

			binding := proposal.Binding
			binding.UserID = f.other.UserID
			binding.ConversationID = f.other.ConversationID
			auditWant(t, f.repo.AuthorizeCreate(
				f.ctx, f.other, run.ID, call.ID, binding,
			), service.ErrAgentRunNotFound)

			auditWant(t, f.repo.FinishCall(
				f.ctx, f.other, run.ID, call.ID, auditReadFinish(),
			), service.ErrAgentRunNotFound)
			auditWant(t, f.repo.FinishRun(
				f.ctx, f.other, run.ID, auditRunFinish(),
			), service.ErrAgentRunNotFound)

			wrong := f.owner
			wrong.ConversationID = f.other.ConversationID
			_, err = f.repo.StartRun(f.ctx, wrong)
			auditWant(t, err, service.ErrConversationNotFound)
			_, err = f.repo.ReadOwned(f.ctx, wrong, run.ID)
			auditWant(t, err, service.ErrConversationNotFound)

			snapshot := f.read(run.ID)
			if snapshot.Run.ToolCalls != 1 ||
				snapshot.Calls[0].WriteAuthorized {
				t.Fatal("foreign request mutated audit")
			}
		}},

		{"sequence_budget", func(f *auditFixture) {
			t := f.t
			run := f.run()

			for i := 1; i <= 5; i++ {
				call := f.call(
					run.ID, "search_rule", fmt.Sprintf("call-%d", i),
				)

				_, err := f.repo.StartCall(
					f.ctx, f.owner, run.ID,
					service.AgentCallStart{
						ToolName: "search_rule", ModelCallID: "overlap",
						ArgumentBytes: 1,
					},
				)
				auditWant(t, err, conflict)

				auditMust(t, f.repo.FinishCall(
					f.ctx, f.owner, run.ID, call.ID, auditReadFinish(),
				))

				_, err = f.repo.StartCall(
					f.ctx, f.owner, run.ID,
					service.AgentCallStart{
						ToolName:      "search_rule",
						ModelCallID:   fmt.Sprintf("call-%d", i),
						ArgumentBytes: 1,
					},
				)
				auditWant(t, err, conflict)
			}

			_, err := f.repo.StartCall(
				f.ctx, f.owner, run.ID,
				service.AgentCallStart{
					ToolName: "search_rule", ModelCallID: "sixth",
					ArgumentBytes: 1,
				},
			)
			auditWant(t, err, conflict)

			snapshot := f.read(run.ID)
			if snapshot.Run.ToolCalls != 5 ||
				len(snapshot.Calls) != 5 {
				t.Fatal("call limit changed stored count")
			}
		}},

		{"terminal_query", func(f *auditFixture) {
			auditTerminal(f, "query_ticket_status", auditReadFinish())
		}},

		{"terminal_proposal", func(f *auditFixture) {
			auditTerminal(f, "create_appeal_ticket", service.AgentCallFinish{
				Status:         "rejected",
				ErrorCode:      "confirmation_required",
				BusinessEffect: "none",
			})
		}},

		{"terminal_failure", func(f *auditFixture) {
			auditTerminal(f, "search_rule", service.AgentCallFinish{
				Status:         "failed",
				ErrorCode:      "unavailable",
				BusinessEffect: "none",
			})
		}},

		{"concurrent_start", func(f *auditFixture) {
			run := f.run()
			auditRace(f.t, func() error {
				_, err := f.repo.StartCall(
					f.ctx, f.owner, run.ID,
					service.AgentCallStart{
						ToolName: "search_rule", ModelCallID: "race",
						ArgumentBytes: 1,
					},
				)
				return err
			})

			snapshot := f.read(run.ID)
			if snapshot.Run.ToolCalls != 1 ||
				len(snapshot.Calls) != 1 ||
				snapshot.Calls[0].Sequence != 1 {
				f.t.Fatal("concurrent start duplicated audit")
			}
		}},

		{"concurrent_authorize", func(f *auditFixture) {
			run := f.run()
			proposal := f.prepare()
			call := f.confirmed(run, proposal)

			auditRace(f.t, func() error {
				return f.repo.AuthorizeCreate(
					f.ctx, f.owner, run.ID, call.ID, proposal.Binding,
				)
			})

			snapshot := f.read(run.ID)
			if !snapshot.Calls[0].WriteAuthorized ||
				snapshot.Calls[0].BusinessEffect != "unknown" ||
				snapshot.Run.BusinessEffect != "unknown" {
				f.t.Fatal("permit was not persisted atomically")
			}
		}},

		{"concurrent_finish_call", func(f *auditFixture) {
			run := f.run()
			call := f.call(run.ID, "search_rule", "race")

			auditRace(f.t, func() error {
				return f.repo.FinishCall(
					f.ctx, f.owner, run.ID, call.ID, auditReadFinish(),
				)
			})

			if f.read(run.ID).Calls[0].Status != "succeeded" {
				f.t.Fatal("call finalization")
			}
		}},

		{"concurrent_finish_run", func(f *auditFixture) {
			run := f.run()
			auditRace(f.t, func() error {
				return f.repo.FinishRun(
					f.ctx, f.owner, run.ID, auditRunFinish(),
				)
			})

			if f.read(run.ID).Run.ExecutionStatus != "completed" {
				f.t.Fatal("run finalization")
			}
		}},

		{"rollback_call_insert", func(f *auditFixture) {
			run := f.run()
			auditTrigger(f.t, f.db)

			_, err := f.repo.StartCall(
				f.ctx, f.owner, run.ID,
				service.AgentCallStart{
					ToolName: "search_rule", ModelCallID: "rollback",
					ArgumentBytes: 1,
				},
			)
			auditWant(f.t, err, service.ErrAgentAuditUnavailable)

			snapshot := f.read(run.ID)
			if snapshot.Run.ToolCalls != 0 ||
				len(snapshot.Calls) != 0 {
				f.t.Fatal("call insert survived failed run update")
			}
		}},

		{"rollback_authorize", func(f *auditFixture) {
			run := f.run()
			proposal := f.prepare()
			call := f.confirmed(run, proposal)
			auditTrigger(f.t, f.db)

			auditWant(f.t, f.repo.AuthorizeCreate(
				f.ctx, f.owner, run.ID, call.ID, proposal.Binding,
			), service.ErrAgentAuditUnavailable)

			snapshot := f.read(run.ID)
			if snapshot.Calls[0].WriteAuthorized ||
				snapshot.Calls[0].BusinessEffect != "none" ||
				snapshot.Run.BusinessEffect != "none" {
				f.t.Fatal("partial permit persisted")
			}
		}},

		{"lock_timeout", func(f *auditFixture) {
			auditLocked(f, false)
		}},

		{"lock_cancel", func(f *auditFixture) {
			auditLocked(f, true)
		}},

		{"commit_and_recover", func(f *auditFixture) {
			t := f.t
			run := f.run()
			proposal := f.prepare()
			call := f.confirmed(run, proposal)

			auditMust(t, f.repo.AuthorizeCreate(
				f.ctx, f.owner, run.ID, call.ID, proposal.Binding,
			))
			ticket := f.create(proposal)

			// 业务已经提交，暂不执行审计收尾，构造中断窗口。
			observed, err := service.ObserveAgentRun(
				f.ctx, f.repo, f.svc, f.owner, run.ID,
			)
			auditMust(t, err)

			if observed.BusinessEffect != "committed" ||
				observed.Ticket == nil ||
				observed.Ticket.TicketID != ticket.TicketID ||
				observed.Stored.Run.BusinessEffect != "unknown" ||
				observed.Stored.Calls[0].Status != "started" {
				t.Fatal("read-only recovery lost committed fact")
			}
			if f.read(run.ID).Calls[0].Status != "started" {
				t.Fatal("observation wrote audit")
			}

			finish := service.AgentCallFinish{
				Status:         "succeeded",
				ErrorCode:      "none",
				BusinessEffect: "committed",
				Ticket:         ticket,
			}
			auditMust(t, f.repo.FinishCall(
				f.ctx, f.owner, run.ID, call.ID, finish,
			))
			auditMust(t, f.repo.FinishRun(
				f.ctx, f.owner, run.ID,
				service.AgentRunFinish{
					Status:     "completed",
					Decision:   "ticket_created",
					ReasonCode: "none",
				},
			))

			snapshot := f.read(run.ID)
			if snapshot.Run.BusinessEffect != "committed" ||
				snapshot.Calls[0].TicketID == nil ||
				*snapshot.Calls[0].TicketID != ticket.TicketID ||
				snapshot.Calls[0].TicketStatus != model.AppealTicketPendingReview {
				t.Fatal("committed receipt not stored")
			}

			auditWant(t, f.repo.FinishCall(
				f.ctx, f.owner, run.ID, call.ID,
				service.AgentCallFinish{
					Status:         "failed",
					ErrorCode:      "unavailable",
					BusinessEffect: "none",
				},
			), conflict)

			replay := f.create(proposal)
			if !replay.Replayed || replay.TicketID != ticket.TicketID {
				t.Fatal("real service replay")
			}

			var count int64
			auditMust(t, f.db.Model(&model.TicketRecord{}).
				Where("ticket_id = ?", ticket.TicketID).
				Count(&count).Error)
			if count != 1 {
				t.Fatal("replay duplicated business record")
			}

			var tickets int64
			auditMust(t, f.db.Model(&model.AppealTicket{}).
				Where(
					"user_id = ? AND idempotency_key = ?",
					f.owner.UserID, proposal.Binding.IdempotencyKey,
				).Count(&tickets).Error)
			if tickets != 1 {
				t.Fatal("replay duplicated ticket")
			}
		}},

		{"pending_absent", func(f *auditFixture) {
			t := f.t
			run := f.run()
			proposal := f.prepare()
			call := f.confirmed(run, proposal)

			auditMust(t, f.repo.AuthorizeCreate(
				f.ctx, f.owner, run.ID, call.ID, proposal.Binding,
			))

			observed, err := service.ObserveAgentRun(
				f.ctx, f.repo, f.svc, f.owner, run.ID,
			)
			auditMust(t, err)

			if observed.BusinessEffect != "unknown" ||
				observed.Ticket != nil ||
				observed.RecoveryCode != "outcome_unknown" {
				t.Fatal("absent ticket treated as rollback")
			}

			auditMust(t, f.repo.FinishCall(
				f.ctx, f.owner, run.ID, call.ID,
				service.AgentCallFinish{
					Status:         "unknown",
					ErrorCode:      "outcome_unknown",
					BusinessEffect: "unknown",
				},
			))
			auditMust(t, f.repo.FinishRun(
				f.ctx, f.owner, run.ID,
				service.AgentRunFinish{
					Status:     "unknown",
					Decision:   "failed",
					ReasonCode: "outcome_unknown",
				},
			))

			if f.read(run.ID).Run.BusinessEffect != "unknown" {
				t.Fatal("unknown fact lost")
			}
		}},

		{"receipt_validation", func(f *auditFixture) {
			t := f.t
			run := f.run()
			proposal := f.prepare()
			call := f.confirmed(run, proposal)

			auditMust(t, f.repo.AuthorizeCreate(
				f.ctx, f.owner, run.ID, call.ID, proposal.Binding,
			))
			ticket := f.create(proposal)

			for _, field := range []string{"id", "status", "time"} {
				copy := *ticket
				switch field {
				case "id":
					copy.TicketID++
				case "status":
					copy.InitialStatus = model.AppealTicketSubmitted
				case "time":
					copy.CreatedAt = copy.CreatedAt.Add(time.Second)
				}

				err := f.repo.FinishCall(
					f.ctx, f.owner, run.ID, call.ID,
					service.AgentCallFinish{
						Status:         "succeeded",
						ErrorCode:      "none",
						BusinessEffect: "committed",
						Ticket:         &copy,
					},
				)
				auditWant(t, err, conflict)
			}

			auditWant(t, f.repo.FinishCall(
				f.ctx, f.owner, run.ID, call.ID,
				service.AgentCallFinish{
					Status:         "failed",
					ErrorCode:      "unavailable",
					BusinessEffect: "none",
				},
			), conflict)

			if f.read(run.ID).Calls[0].Status != "started" {
				t.Fatal("invalid receipt changed audit")
			}

			auditMust(t, f.repo.FinishCall(
				f.ctx, f.owner, run.ID, call.ID,
				service.AgentCallFinish{
					Status:         "succeeded",
					ErrorCode:      "none",
					BusinessEffect: "committed",
					Ticket:         ticket,
				},
			))
		}},

		{"permission_checks", func(f *auditFixture) {
			t := f.t
			run := f.run()
			proposal := f.prepare()
			call := f.confirmed(run, proposal)

			wrong := proposal.Binding
			wrong.RequestHash = strings.Repeat("b", 64)
			if wrong.RequestHash == proposal.Binding.RequestHash {
				wrong.RequestHash = strings.Repeat("c", 64)
			}

			auditWant(t, f.repo.AuthorizeCreate(
				f.ctx, f.owner, run.ID, call.ID, wrong,
			), conflict)

			auditWant(t, f.repo.FinishCall(
				f.ctx, f.owner, run.ID, call.ID,
				service.AgentCallFinish{
					Status:         "unknown",
					ErrorCode:      "outcome_unknown",
					BusinessEffect: "unknown",
				},
			), conflict)

			auditWant(t, f.repo.FinishCall(
				f.ctx, f.owner, run.ID, call.ID, auditReadFinish(),
			), conflict)

			if f.read(run.ID).Calls[0].WriteAuthorized {
				t.Fatal("bad binding authorized")
			}

			auditMust(t, f.repo.AuthorizeCreate(
				f.ctx, f.owner, run.ID, call.ID, proposal.Binding,
			))
			auditWant(t, f.repo.AuthorizeCreate(
				f.ctx, f.owner, run.ID, call.ID, proposal.Binding,
			), conflict)

			auditMust(t, f.repo.FinishCall(
				f.ctx, f.owner, run.ID, call.ID,
				service.AgentCallFinish{
					Status:         "rejected",
					ErrorCode:      "precondition_changed",
					BusinessEffect: "none",
				},
			))

			snapshot := f.read(run.ID)
			if snapshot.Run.BusinessEffect != "none" ||
				snapshot.Calls[0].BusinessEffect != "none" {
				t.Fatal("known no-write outcome")
			}
		}},

		{"cross_run_call", func(f *auditFixture) {
			t := f.t
			first, second := f.run(), f.run()
			proposal := f.prepare()
			call := f.confirmed(first, proposal)

			auditWant(t, f.repo.AuthorizeCreate(
				f.ctx, f.owner, second.ID, call.ID, proposal.Binding,
			), conflict)

			auditWant(t, f.repo.FinishCall(
				f.ctx, f.owner, second.ID, call.ID, auditReadFinish(),
			), conflict)

			if f.read(first.ID).Calls[0].Status != "started" ||
				len(f.read(second.ID).Calls) != 0 {
				t.Fatal("cross-run mutation")
			}
		}},

		{"unfinished_run", func(f *auditFixture) {
			t := f.t
			run := f.run()
			f.call(run.ID, "search_rule", "pending")

			auditWant(t, f.repo.FinishRun(
				f.ctx, f.owner, run.ID, auditRunFinish(),
			), conflict)

			auditWant(t, f.repo.FinishRun(
				f.ctx, f.owner, run.ID,
				service.AgentRunFinish{
					Status:     "failed",
					Decision:   "ticket_created",
					ReasonCode: "unavailable",
				},
			), conflict)

			if f.read(run.ID).Run.ExecutionStatus != "running" {
				t.Fatal("unfinished run incorrectly closed")
			}
		}},

		{"cancel_before_start", func(f *auditFixture) {
			ctx, cancel := context.WithCancel(f.ctx)
			cancel()

			_, err := f.repo.StartRun(ctx, f.owner)
			auditWant(f.t, err, context.Canceled)

			var count int64
			auditMust(f.t, f.db.Model(&model.AgentRun{}).
				Where("user_id = ?", f.owner.UserID).
				Count(&count).Error)
			if count != 0 {
				f.t.Fatal("canceled start persisted")
			}
		}},

		{"configuration", func(f *auditFixture) {
			t := f.t

			_, err := NewAgentAuditRepositoryWithClock(nil, time.Now)
			auditWant(t, err, service.ErrAgentAuditConfiguration)

			_, err = NewAgentAuditRepositoryWithClock(f.db, nil)
			auditWant(t, err, service.ErrAgentAuditConfiguration)

			_, err = NewAgentAuditRepository(
				f.db.Session(&gorm.Session{DryRun: true}),
			)
			auditWant(t, err, service.ErrAgentAuditConfiguration)

			tx := f.db.Begin()
			auditMust(t, tx.Error)
			_, err = NewAgentAuditRepository(tx)
			auditMust(t, tx.Rollback().Error)
			auditWant(t, err, service.ErrAgentAuditConfiguration)

			repo, err := NewAgentAuditRepositoryWithClock(
				f.db, func() time.Time { return time.Time{} },
			)
			auditMust(t, err)
			_, err = repo.StartRun(f.ctx, f.owner)
			auditWant(t, err, service.ErrAgentAuditConfiguration)
		}},

		{"privacy", func(f *auditFixture) {
			t := f.t
			run := f.run()
			proposal := f.prepare()
			call := f.confirmed(run, proposal)
			snapshot := f.read(run.ID)

			for _, value := range []any{
				snapshot, snapshot.Run, snapshot.Calls[0], *call,
			} {
				raw, err := json.Marshal(value)
				auditMust(t, err)
				if string(raw) != "{}" {
					t.Fatal("raw audit is JSON-visible")
				}
			}

			var columns []string
			auditMust(t, f.db.Raw(
				"SELECT column_name FROM information_schema.columns "+
					"WHERE table_schema=DATABASE() "+
					"AND table_name IN ('agent_runs','tool_calls')",
			).Scan(&columns).Error)

			for _, column := range columns {
				switch column {
				case "arguments", "token", "confirmation_token",
					"reason", "subject_ref", "raw_response":
					t.Fatalf("raw private column: %s", column)
				}
			}

			if snapshot.Calls[0].ModelCallID != nil ||
				!snapshot.Calls[0].Confirmed ||
				snapshot.Calls[0].OperationKey != proposal.Binding.IdempotencyKey ||
				snapshot.Calls[0].RequestHash != proposal.Binding.RequestHash ||
				snapshot.Calls[0].PolicyDigest != proposal.Binding.PolicyDigest {
				t.Fatal("private binding storage")
			}
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.run(auditSeed(t, db))
		})
	}
}

func auditNumber(t *testing.T, err error, number uint16) {
	t.Helper()
	var mysqlErr *driver.MySQLError
	if !errors.As(err, &mysqlErr) || mysqlErr.Number != number {
		t.Fatalf("want MySQL %d got %v", number, err)
	}
}

func auditTerminal(
	f *auditFixture,
	name string,
	finish service.AgentCallFinish,
) {
	f.t.Helper()

	run := f.run()
	call := f.call(run.ID, name, "terminal")
	auditMust(f.t, f.repo.FinishCall(
		f.ctx, f.owner, run.ID, call.ID, finish,
	))

	_, err := f.repo.StartCall(
		f.ctx, f.owner, run.ID,
		service.AgentCallStart{
			ToolName: "search_rule", ModelCallID: "after",
			ArgumentBytes: 1,
		},
	)
	auditWant(f.t, err, service.ErrAgentAuditConflict)

	auditWant(f.t, f.repo.FinishCall(
		f.ctx, f.owner, run.ID, call.ID, finish,
	), service.ErrAgentAuditConflict)

	if len(f.read(run.ID).Calls) != 1 {
		f.t.Fatal("terminal call followed by extra work")
	}
}

func auditLocked(f *auditFixture, cancel bool) {
	t := f.t
	run := f.run()

	tx := f.db.WithContext(f.ctx).Begin()
	auditMust(t, tx.Error)
	defer tx.Rollback()

	var locked model.AgentRun
	auditMust(t, tx.Clauses(
		clause.Locking{Strength: "UPDATE"},
	).Where("id = ?", run.ID).Take(&locked).Error)

	ctx := f.ctx
	want := service.ErrAgentAuditUnavailable
	if cancel {
		var release context.CancelFunc
		ctx, release = context.WithTimeout(
			ctx, 100*time.Millisecond,
		)
		defer release()
		want = context.DeadlineExceeded
	}

	_, err := f.repo.StartCall(
		ctx, f.owner, run.ID,
		service.AgentCallStart{
			ToolName: "search_rule", ModelCallID: "blocked",
			ArgumentBytes: 1,
		},
	)
	auditWant(t, err, want)
	auditMust(t, tx.Rollback().Error)

	snapshot := f.read(run.ID)
	if snapshot.Run.ToolCalls != 0 || len(snapshot.Calls) != 0 {
		t.Fatal("blocked transaction partially persisted")
	}

	f.call(run.ID, "search_rule", "after-release")
}

func auditSchema(t *testing.T, db *gorm.DB) {
	t.Helper()

	for _, row := range []struct {
		index   string
		columns string
	}{
		{"uk_tool_call_sequence", "run_id,sequence"},
		{"uk_tool_call_model", "run_id,model_call_id"},
	} {
		var columns string
		auditMust(t, db.Raw(
			"SELECT COALESCE(GROUP_CONCAT("+
				"column_name ORDER BY seq_in_index SEPARATOR ','),'') "+
				"FROM information_schema.statistics "+
				"WHERE table_schema=DATABASE() "+
				"AND table_name='tool_calls' "+
				"AND index_name=? AND non_unique=0",
			row.index,
		).Scan(&columns).Error)

		if columns != row.columns {
			t.Fatalf("index %s=%s", row.index, columns)
		}
	}

	for _, table := range []string{"agent_runs", "tool_calls"} {
		var engine string
		auditMust(t, db.Raw(
			"SELECT engine FROM information_schema.tables "+
				"WHERE table_schema=DATABASE() AND table_name=?",
			table,
		).Scan(&engine).Error)
		if engine != "InnoDB" {
			t.Fatal("nontransactional audit table")
		}
	}

	for _, row := range []struct {
		table  string
		column string
	}{
		{"agent_runs", "id"},
		{"tool_calls", "run_id"},
		{"tool_calls", "model_call_id"},
	} {
		var collation string
		auditMust(t, db.Raw(
			"SELECT collation_name FROM information_schema.columns "+
				"WHERE table_schema=DATABASE() "+
				"AND table_name=? AND column_name=?",
			row.table, row.column,
		).Scan(&collation).Error)
		if collation != "ascii_bin" {
			t.Fatal("identifier collation")
		}
	}

	var precision int
	auditMust(t, db.Raw(
		"SELECT datetime_precision FROM information_schema.columns "+
			"WHERE table_schema=DATABASE() "+
			"AND table_name='tool_calls' AND column_name='started_at'",
	).Scan(&precision).Error)

	if precision != 3 {
		t.Fatal("audit timestamp precision")
	}

	t.Log("MYSQL_SCHEMA audit indexes, engine, collation and timestamp checked")
}
