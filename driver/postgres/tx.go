package postgres

import (
	"context"
	"database/sql/driver"
)

// pgTx pins a pgConn for the lifetime of a transaction. database/sql owns the
// pinning; we just issue COMMIT / ROLLBACK here.
type pgTx struct {
	conn *pgConn
}

var _ driver.Tx = (*pgTx)(nil)

// Commit ends the transaction whatever it returns. If a streaming Rows of this
// transaction is still open (see [ErrRowsOpen]), Commit returns ErrRowsOpen and
// the transaction is rolled back: close Rows before Commit. Nothing is sent in
// that case; a [Tx] of a [Pool] discards the connection, which makes the server roll
// the transaction back, and database/sql, which closes a Tx's Rows before it calls
// Commit and drops a connection that is inside a transaction, never gets here
// with one open.
func (t *pgTx) Commit() error {
	if t.conn == nil {
		return ErrClosed
	}
	rolledBack, err := t.conn.simpleExecCmd(context.Background(), "COMMIT", true)
	if err == nil && rolledBack {
		// The transaction was in the aborted state: the server treated the COMMIT
		// as a ROLLBACK and reported no error. The transaction is over and the
		// session is out of it. sessionDirty stays as BeginTx set it, so under
		// database/sql ResetSession still resets the session before the conn is
		// reused; the native Pool never calls ResetSession, and nothing depends on
		// it there (the aborted transaction was rolled back, the session is idle).
		return ErrTxCommitRollback
	}
	if err == nil {
		// After a successful COMMIT the session is back to a clean state —
		// no temp tables, no pending SET, no transaction. Clear the dirty
		// flag so ResetSession skips the expensive DISCARD ALL round-trip.
		t.conn.sessionDirty.Store(false)
	}
	return err
}

func (t *pgTx) Rollback() error {
	if t.conn == nil {
		return ErrClosed
	}
	err := t.conn.simpleExecNoTag(context.Background(), "ROLLBACK")
	if err == nil {
		// Same as Commit: a successful ROLLBACK restores the session to a
		// clean state.
		t.conn.sessionDirty.Store(false)
	}
	return err
}
