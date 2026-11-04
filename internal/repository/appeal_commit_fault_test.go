//go:build day11mysql

package repository

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"video-support-agent/internal/model"
	"video-support-agent/internal/service"

	mysql "github.com/go-sql-driver/mysql"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var (
	day11LostAck   = errors.New("TEST_ONLY: commit response lost")
	day11ReadFault = errors.New("TEST_ONLY: recovery read unavailable")
)

// 故障只存在于本测试驱动；SQL、INSERT、COMMIT/ROLLBACK仍到真实MySQL。
type day11CommitFault struct {
	mode                                                string
	after                                               func() error
	entered, gate, finished                             chan struct{}
	releaseOnce                                         sync.Once
	inCommit, committed, rolledBack, failReads          atomic.Bool
	commits, rollbacks, readsDuringCommit, readFailures atomic.Int32
}

func day11NewCommitFault(mode string) *day11CommitFault {
	return &day11CommitFault{
		mode: mode, entered: make(chan struct{}),
		gate: make(chan struct{}), finished: make(chan struct{}),
	}
}

func (f *day11CommitFault) release() {
	f.releaseOnce.Do(func() { close(f.gate) })
}

func (f *day11CommitFault) waitGate() error {
	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()

	select {
	case <-f.gate:
		return nil
	case <-timer.C:
		return errors.New("TEST_ONLY: fault gate not released")
	}
}

type day11FaultConnector struct {
	driver.Connector
	fault *day11CommitFault
}

func (c *day11FaultConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &day11FaultConn{Conn: conn, fault: c.fault}, nil
}

type day11FaultConn struct {
	driver.Conn
	fault *day11CommitFault
}

func (c *day11FaultConn) BeginTx(
	ctx context.Context, opts driver.TxOptions,
) (driver.Tx, error) {
	tx, err := c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &day11FaultTx{Tx: tx, fault: c.fault}, nil
}

func (c *day11FaultConn) PrepareContext(
	ctx context.Context, query string,
) (driver.Stmt, error) {
	return c.Conn.(driver.ConnPrepareContext).PrepareContext(ctx, query)
}

func (c *day11FaultConn) ExecContext(
	ctx context.Context, query string, args []driver.NamedValue,
) (driver.Result, error) {
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
}

func (c *day11FaultConn) QueryContext(
	ctx context.Context, query string, args []driver.NamedValue,
) (driver.Rows, error) {
	if c.fault.inCommit.Load() {
		c.fault.readsDuringCommit.Add(1)
	}
	if c.fault.failReads.Load() &&
		strings.Contains(strings.ToLower(query), "appeal_tickets") {
		c.fault.readFailures.Add(1)
		return nil, day11ReadFault
	}
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
}

func (c *day11FaultConn) Ping(ctx context.Context) error {
	return c.Conn.(driver.Pinger).Ping(ctx)
}

func (c *day11FaultConn) CheckNamedValue(v *driver.NamedValue) error {
	return c.Conn.(driver.NamedValueChecker).CheckNamedValue(v)
}

func (c *day11FaultConn) ResetSession(ctx context.Context) error {
	return c.Conn.(driver.SessionResetter).ResetSession(ctx)
}

func (c *day11FaultConn) IsValid() bool {
	return c.Conn.(driver.Validator).IsValid()
}

type day11FaultTx struct {
	driver.Tx
	fault *day11CommitFault
}

func (tx *day11FaultTx) Rollback() error {
	tx.fault.rollbacks.Add(1)
	return tx.Tx.Rollback()
}

func (tx *day11FaultTx) Commit() error {
	f := tx.fault
	f.commits.Add(1)
	f.inCommit.Store(true)
	defer func() {
		f.inCommit.Store(false)
		close(f.finished)
	}()
	close(f.entered)

	if f.mode == "hold_before" {
		if err := f.waitGate(); err != nil {
			_ = tx.Tx.Rollback()
			return err
		}
	}

	if f.mode == "absent" || f.mode == "conflict" {
		if err := tx.Tx.Rollback(); err != nil {
			return err
		}
		f.rolledBack.Store(true)
	} else {
		if err := tx.Tx.Commit(); err != nil {
			return err
		}
		f.committed.Store(true)
	}

	if f.after != nil {
		if err := f.after(); err != nil {
			return err
		}
	}
	if f.mode == "hold_after" {
		if err := f.waitGate(); err != nil {
			return err
		}
	}
	if f.mode == "read_error" {
		f.failReads.Store(true)
	}

	switch f.mode {
	case "lost_ack", "absent", "conflict", "read_error":
		return day11LostAck
	default:
		return nil
	}
}

func day11FaultOpen(
	t *testing.T, cfg *mysql.Config, fault *day11CommitFault,
) (*AppealTicketRepository, *sql.DB) {
	t.Helper()

	base, err := mysql.NewConnector(cfg)
	day11SQLMust(t, err)

	pool := sql.OpenDB(&day11FaultConnector{
		Connector: base, fault: fault,
	})
	pool.SetMaxOpenConns(2)
	pool.SetMaxIdleConns(2)
	t.Cleanup(func() { day11SQLMust(t, pool.Close()) })

	db, err := gorm.Open(
		gormmysql.New(gormmysql.Config{Conn: pool}),
		&gorm.Config{
			Logger:               logger.Default.LogMode(logger.Silent),
			DisableAutomaticPing: true,
		},
	)
	day11SQLMust(t, err)
	day11SQLMust(t, pool.PingContext(day11SQLContext(t)))
	return day11SQLRepo(t, db), pool
}

func day11RecoveryService(
	t *testing.T, r *AppealTicketRepository,
) *service.AppealTicketService {
	t.Helper()

	eligibility, err := service.NewAppealEligibilityService(
		service.NoAppealPolicies{},
	)
	day11SQLMust(t, err)

	svc, err := service.NewAppealTicketServiceWithClock(
		r, eligibility, nil, func() time.Time { return day11SQLAt },
	)
	day11SQLMust(t, err)
	return svc
}

func day11Recover(
	t *testing.T,
	svc *service.AppealTicketService,
	c service.AppealCreateCommand,
) (*service.AppealCreateResult, error) {
	t.Helper()

	return svc.Recover(
		day11SQLContext(t), c.UserID, c.ConversationID,
		c.IdempotencyKey, c.RequestHash,
	)
}

func day11RequireUnknown(
	t *testing.T, result *service.AppealCreateResult, err error,
) {
	t.Helper()

	if result != nil || !errors.Is(err, service.ErrAppealOutcomeUnknown) {
		t.Fatalf("expected nil/unknown, result=%+v err=%v", result, err)
	}
}

func day11RequireReplay(
	t *testing.T, result *service.AppealCreateResult, err error,
) {
	t.Helper()
	day11SQLMust(t, err)

	if result == nil || result.TicketID == 0 || !result.Replayed ||
		result.InitialStatus != model.AppealTicketPendingReview ||
		!result.CreatedAt.Equal(day11SQLAt) {
		t.Fatalf("invalid recovered result=%+v", result)
	}
}

func day11FaultDrain(
	t *testing.T, pool *sql.DB, fault *day11CommitFault,
) {
	t.Helper()

	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()

	for pool.Stats().InUse != 0 {
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatalf("connection still in use: %+v", pool.Stats())
		}
	}

	if fault.commits.Load() != 1 ||
		fault.rollbacks.Load() != 0 ||
		fault.readsDuringCommit.Load() != 0 {
		t.Fatalf("commit=%d rollback=%d reads_during_commit=%d",
			fault.commits.Load(),
			fault.rollbacks.Load(),
			fault.readsDuringCommit.Load())
	}

	day11SQLMust(t, pool.Close())
	if pool.Stats().OpenConnections != 0 {
		t.Fatal("fault pool did not close")
	}

	t.Logf(
		"MYSQL_FAULT mode=%s committed=%v rolled_back=%v commits=1 concurrent_rollback=0 concurrent_read=0 pool_closed=true",
		fault.mode, fault.committed.Load(), fault.rolledBack.Load(),
	)
}

func TestDay11CommitFault(t *testing.T) {
	db, cfg := day11SQLDatabase(t)
	day11SQLMigrate(t, db)
	fixture := day11SQLSeed(t, db, cfg)
	healthy := day11SQLRepo(t, db)
	recovery := day11RecoveryService(t, healthy)

	for _, mode := range []string{
		"lost_ack", "cancel_after", "absent", "read_error", "conflict",
	} {
		t.Run(mode, func(t *testing.T) {
			fault := day11NewCommitFault(mode)
			repo, pool := day11FaultOpen(t, cfg, fault)
			c := fixture.command(t)
			ctx, cancel := context.WithCancel(day11SQLContext(t))
			defer cancel()

			other := c
			var winner *service.AppealCreateResult

			if mode == "cancel_after" {
				fault.after = func() error {
					cancel()
					return nil
				}
			}
			if mode == "conflict" {
				other.Input.Reason += " other accepted request"
				var err error
				other.RequestHash, err = service.AppealRequestHash(
					other.ConversationID, other.Input,
				)
				day11SQLMust(t, err)

				fault.after = func() error {
					work, stop := context.WithTimeout(
						context.Background(), 5*time.Second,
					)
					defer stop()

					var err error
					winner, err = healthy.CreateWithRecord(
						work, other, day11SQLCheck,
					)
					return err
				}
			}

			result, err := repo.CreateWithRecord(ctx, c, day11SQLCheck)

			switch mode {
			case "lost_ack":
				day11RequireReplay(t, result, err)

			case "cancel_after":
				day11SQLMust(t, err)
				if result == nil || result.Replayed ||
					ctx.Err() != context.Canceled {
					t.Fatalf("known commit lost: result=%+v ctx=%v",
						result, ctx.Err())
				}

			case "absent":
				day11RequireUnknown(t, result, err)
				if !errors.Is(err, day11LostAck) {
					t.Fatal("fault not observed")
				}
				day11SQLCounts(t, db, c, 0, 0)

				missing, recoverErr := day11Recover(t, recovery, c)
				day11RequireUnknown(t, missing, recoverErr)
				day11SQLCounts(t, db, c, 0, 0)

				// 只有这次显式调用才重新创建，Recover本身不写。
				_, retryErr := healthy.CreateWithRecord(
					day11SQLContext(t), c, day11SQLCheck,
				)
				day11SQLMust(t, retryErr)

			case "read_error":
				day11RequireUnknown(t, result, err)
				if !errors.Is(err, day11ReadFault) ||
					!errors.Is(err, day11LostAck) ||
					fault.readFailures.Load() == 0 {
					t.Fatalf("recovery failure not observed: %v", err)
				}

				failed, recoverErr := day11Recover(
					t, day11RecoveryService(t, repo), c,
				)
				day11RequireUnknown(t, failed, recoverErr)
				if !errors.Is(recoverErr, day11ReadFault) {
					t.Fatal("public read failure lost")
				}
				fault.failReads.Store(false)

			case "conflict":
				if result != nil ||
					!errors.Is(err, service.ErrAppealIdempotencyConflict) ||
					winner == nil {
					t.Fatalf("conflict result=%+v err=%v winner=%+v",
						result, err, winner)
				}

				rejected, recoverErr := day11Recover(t, recovery, c)
				if rejected != nil ||
					!errors.Is(recoverErr, service.ErrAppealIdempotencyConflict) {
					t.Fatalf("public conflict=%v", recoverErr)
				}
				c = other
			}

			if mode == "absent" || mode == "conflict" {
				if !fault.rolledBack.Load() || fault.committed.Load() {
					t.Fatal("expected real rollback")
				}
			} else if !fault.committed.Load() {
				t.Fatal("real commit not established")
			}

			recovered, recoverErr := day11Recover(t, recovery, c)
			day11RequireReplay(t, recovered, recoverErr)
			if result != nil && recovered.TicketID != result.TicketID {
				t.Fatal("recovery ID changed")
			}
			if winner != nil && recovered.TicketID != winner.TicketID {
				t.Fatal("winner ID changed")
			}

			again, replayErr := healthy.CreateWithRecord(
				day11SQLContext(t), c, nil,
			)
			day11RequireReplay(t, again, replayErr)
			if again.TicketID != recovered.TicketID {
				t.Fatal("same-key retry duplicated ticket")
			}

			day11SQLCounts(t, db, c, 1, 1)
			day11FaultDrain(t, pool, fault)
		})
	}

	for _, mode := range []string{"hold_before", "hold_after"} {
		t.Run(mode, func(t *testing.T) {
			fault := day11NewCommitFault(mode)
			repo, pool := day11FaultOpen(t, cfg, fault)
			c := fixture.command(t)

			ctx, cancel := context.WithTimeout(
				context.Background(), 5*time.Second,
			)
			defer cancel()

			resultCh := make(chan day11SQLResult, 1)
			callDone := make(chan struct{})

			t.Cleanup(func() {
				fault.release()
				cancel()

				select {
				case <-callDone:
				case <-time.After(7 * time.Second):
					t.Error("create worker not drained")
				}
				if fault.commits.Load() != 0 {
					select {
					case <-fault.finished:
					case <-time.After(7 * time.Second):
						t.Error("commit worker not drained")
					}
				}
			})

			started := time.Now()
			go func() {
				defer close(callDone)
				result, err := repo.CreateWithRecord(
					ctx, c, day11SQLCheck,
				)
				resultCh <- day11SQLResult{result: result, err: err}
			}()

			select {
			case <-fault.entered:
			case early := <-resultCh:
				t.Fatalf("commit not reached: %v", early.err)
			case <-time.After(7 * time.Second):
				t.Fatal("commit not entered")
			}

			var outcome day11SQLResult
			select {
			case outcome = <-resultCh:
			case <-time.After(7 * time.Second):
				t.Fatal("creation did not return within deadline bound")
			}

			day11RequireUnknown(t, outcome.result, outcome.err)
			if !errors.Is(outcome.err, context.DeadlineExceeded) ||
				time.Since(started) > 7*time.Second {
				t.Fatalf("deadline contract: elapsed=%s err=%v",
					time.Since(started), outcome.err)
			}

			select {
			case <-fault.finished:
				t.Fatal("commit unexpectedly settled before gate release")
			default:
			}

			if fault.rollbacks.Load() != 0 ||
				fault.readsDuringCommit.Load() != 0 {
				t.Fatal("automatic rollback or recovery raced with pending commit")
			}

			early, recoverErr := day11Recover(t, recovery, c)
			if mode == "hold_before" {
				if fault.committed.Load() {
					t.Fatal("commit sent before gate release")
				}
				day11RequireUnknown(t, early, recoverErr)
				day11SQLCounts(t, db, c, 0, 0)
			} else {
				if !fault.committed.Load() {
					t.Fatal("server commit not completed")
				}
				day11RequireReplay(t, early, recoverErr)
				day11SQLCounts(t, db, c, 1, 1)
			}

			t.Logf(
				"MYSQL_PENDING mode=%s public=outcome_unknown driver_pending=true elapsed_ms=%d",
				mode, time.Since(started).Milliseconds(),
			)

			fault.release()
			select {
			case <-fault.finished:
			case <-time.After(7 * time.Second):
				t.Fatal("commit did not settle after release")
			}

			if !fault.committed.Load() {
				t.Fatal("late real commit failed")
			}

			recovered, recoverErr := day11Recover(t, recovery, c)
			day11RequireReplay(t, recovered, recoverErr)
			if early != nil && recovered.TicketID != early.TicketID {
				t.Fatal("late recovery ID changed")
			}

			again, replayErr := healthy.CreateWithRecord(
				day11SQLContext(t), c, nil,
			)
			day11RequireReplay(t, again, replayErr)
			if again.TicketID != recovered.TicketID {
				t.Fatal("late retry duplicated ticket")
			}

			day11SQLCounts(t, db, c, 1, 1)
			day11FaultDrain(t, pool, fault)
		})
	}
}
