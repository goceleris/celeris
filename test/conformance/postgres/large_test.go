//go:build postgres

package postgres_test

import (
	"context"
	"testing"
	"time"
)

// TestLargeResultSet streams 100k rows from generate_series, using the simple
// protocol so the driver's row buffer accumulator is exercised.
func TestLargeResultSet(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping large-result test in -short mode")
	}
	db := openDB(t)

	const n = 100_000
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	rows, err := db.QueryContext(ctx, "SELECT i, 'row_' || i AS name FROM generate_series(1,$1) AS i", n)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var i int
		var name string
		if err := rows.Scan(&i, &name); err != nil {
			t.Fatalf("Scan at row %d: %v", count, err)
		}
		count++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	if count != n {
		t.Fatalf("streamed %d rows, want %d", count, n)
	}
}

// TestLargePayloadColumn sends a ~1 MiB text column round-trip.
func TestLargePayloadColumn(t *testing.T) {
	db := openDB(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	payload := make([]byte, 1<<20)
	for i := range payload {
		payload[i] = byte('a' + (i % 26))
	}
	var got string
	if err := db.QueryRowContext(ctx, "SELECT $1::text", string(payload)).Scan(&got); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(got) != len(payload) {
		t.Fatalf("got %d bytes, want %d", len(got), len(payload))
	}
}
