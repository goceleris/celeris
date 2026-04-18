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

func (t *pgTx) Commit() error {
	if t.conn == nil {
		return ErrClosed
	}
	err := t.conn.simpleExecNoTag(context.Background(), "COMMIT")
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
