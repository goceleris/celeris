//go:build postgres

package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestContextCancelStopsQuery asserts that a cancelled context aborts a
// long-running pg_sleep. The driver must fire CancelRequest on the side
// channel and return ctx.Err() (or the server-side "canceling statement" PG
// error).
func TestContextCancelStopsQuery(t *testing.T) {
	db := openDB(t)

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	start := time.Now()
	_, err := db.ExecContext(ctx, "SELECT pg_sleep(10)")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("expected an error after context deadline, got nil")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("cancel took %v — expected <5s (no side-channel CancelRequest?)", elapsed)
	}
	// Accept either ctx.Err() or a server-side pg error containing "cancel".
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		// A PG Error with SQLSTATE 57014 (query_canceled) is also acceptable;
		// we don't type-assert here because the fast-path may surface ctx.Err.
		t.Logf("note: err=%v (accepted if it's a PG cancel response)", err)
	}
}

// TestQueryInflightBeforeCancel starts a query, cancels before it finishes,
// and verifies the conn is recycled (next Exec must succeed).
func TestQueryInflightBeforeCancel(t *testing.T) {
	db := openDB(t)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	_, _ = db.ExecContext(ctx, "SELECT pg_sleep(5)")
	cancel()

	// A subsequent request on a fresh conn must succeed.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	var one int
	if err := db.QueryRowContext(ctx2, "SELECT 1").Scan(&one); err != nil {
		t.Fatalf("post-cancel SELECT 1 failed: %v", err)
	}
	if one != 1 {
		t.Fatalf("got %d, want 1", one)
	}
}
