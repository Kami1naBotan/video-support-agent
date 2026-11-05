package repository

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"testing"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"video-support-agent/internal/knowledgeworker"
)

// This driver supplies transaction plumbing only, without network or SQL data.
// The tests exercise the production timeout/error boundary, not MySQL atomicity.
type timeoutBoundaryConnector struct{}
type timeoutBoundaryDriver struct{}
type timeoutBoundaryConn struct{}
type timeoutBoundaryTx struct{}

func (timeoutBoundaryConnector) Connect(context.Context) (driver.Conn, error) {
	return timeoutBoundaryConn{}, nil
}
func (timeoutBoundaryConnector) Driver() driver.Driver         { return timeoutBoundaryDriver{} }
func (timeoutBoundaryDriver) Open(string) (driver.Conn, error) { return timeoutBoundaryConn{}, nil }
func (timeoutBoundaryConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("SQL is not supported by timeout boundary fixture")
}
func (timeoutBoundaryConn) Close() error              { return nil }
func (timeoutBoundaryConn) Begin() (driver.Tx, error) { return timeoutBoundaryTx{}, nil }
func (timeoutBoundaryTx) Commit() error               { return nil }
func (timeoutBoundaryTx) Rollback() error             { return nil }

func timeoutBoundaryRepository(t *testing.T) *KnowledgeBuildRepository {
	t.Helper()
	pool := sql.OpenDB(timeoutBoundaryConnector{})
	t.Cleanup(func() { _ = pool.Close() })
	db, err := gorm.Open(mysql.New(mysql.Config{
		Conn: pool, SkipInitializeWithVersion: true,
	}), &gorm.Config{DisableAutomaticPing: true})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := NewKnowledgeBuildRepository(db, 10)
	if err != nil {
		t.Fatal(err)
	}
	return repo
}

func TestBuildTransactionLocalTimeoutIsPersistenceUnknown(t *testing.T) {
	repo := timeoutBoundaryRepository(t)
	parent := context.Background()
	err := repo.transaction(parent, func(tx *gorm.DB) error {
		<-tx.Statement.Context.Done()
		return tx.Statement.Context.Err()
	})
	if parent.Err() != nil {
		t.Fatal("parent unexpectedly cancelled")
	}
	if !errors.Is(err, knowledgeworker.ErrBuildStore) {
		t.Fatalf("local transaction timeout = %v; want ErrBuildStore, not a confirmed business timeout", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("persistence uncertainty must not also match a business deadline")
	}
}

func TestBuildTransactionPreservesParentCancellation(t *testing.T) {
	repo := timeoutBoundaryRepository(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := repo.transaction(ctx, func(*gorm.DB) error {
		t.Fatal("cancelled transaction callback must not run")
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v; want context.Canceled", err)
	}
}

func TestBuildTransactionPreservesStateDecision(t *testing.T) {
	repo := timeoutBoundaryRepository(t)
	err := repo.transaction(context.Background(), func(*gorm.DB) error {
		return knowledgeworker.ErrBuildObsolete
	})
	if !errors.Is(err, knowledgeworker.ErrBuildObsolete) {
		t.Fatalf("got %v; want ErrBuildObsolete", err)
	}
}
