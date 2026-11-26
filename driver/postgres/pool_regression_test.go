package postgres

import "testing"

// TestTxDoneAfterFailedCommit used to pin the opposite (a failed Commit left done=false so a
// deferred Rollback could still fire, and the conn stayed pinned until it did). A Commit or
// Rollback now ends the Tx whatever it returns: see TestTxCommitEndsTheTx and
// TestTxFailedRollbackEndsTheTx in tx_ends_on_commit_test.go.

// TestTxDoneAfterSuccessfulCommit verifies done is true after Commit succeeds.
// This is the inverse check: a successful Commit must prevent double-Rollback.
func TestTxDoneAfterSuccessfulCommit(t *testing.T) {
	tx := &Tx{}
	tx.done.Store(true) // simulate post-successful-Commit
	err := tx.Rollback()
	if err == nil || err.Error() != "celeris-postgres: tx already finished" {
		t.Fatalf("expected 'tx already finished', got %v", err)
	}
}
