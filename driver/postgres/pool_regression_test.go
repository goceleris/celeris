package postgres

import (
	"context"
	"errors"
	"testing"

	"github.com/goceleris/celeris/driver/internal/async"
)

// TestTxDoneAfterFailedCommit verifies that a failed Commit leaves done=false
// so a deferred Rollback can still fire. Bug 8: previously, Commit set
// done=true unconditionally before calling inner.Commit(), so a deferred
// Rollback would get "tx already finished" even when Commit failed.
func TestTxDoneAfterFailedCommit(t *testing.T) {
	// A Tx with done=false must run Rollback past the done guard. The pinned
	// conn is a closed pgTx (inner.conn == nil, so inner.Rollback returns
	// ErrClosed without touching a wire) over a zero pgConn and an empty pool,
	// so the call completes without faulting: this test used to rely on a
	// deliberate nil dereference recovered by a defer, which kills the test
	// binary on Windows when the hardware fault is not turned into a panic.
	pool := &Pool{inner: async.NewPool[*pgConn](async.PoolConfig{NumWorkers: 1},
		func(context.Context, int) (*pgConn, error) { return nil, errors.New("unused") })}
	tx := &Tx{inner: &pgTx{}, conn: &pgConn{}, pool: pool}
	if tx.done {
		t.Fatal("done should start false")
	}
	err := tx.Rollback()
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("Rollback with done=false: got %v, want the inner ErrClosed (past the done guard)", err)
	}
	if !tx.done {
		t.Fatal("done should be true after Rollback ran")
	}
	if err := tx.Rollback(); err == nil || err.Error() != "celeris-postgres: tx already finished" {
		t.Fatalf("second Rollback: got %v, want 'tx already finished'", err)
	}
}

// TestTxDoneAfterSuccessfulCommit verifies done is true after Commit succeeds.
// This is the inverse check: a successful Commit must prevent double-Rollback.
func TestTxDoneAfterSuccessfulCommit(t *testing.T) {
	tx := &Tx{done: true} // simulate post-successful-Commit
	err := tx.Rollback()
	if err == nil || err.Error() != "celeris-postgres: tx already finished" {
		t.Fatalf("expected 'tx already finished', got %v", err)
	}
}
