//go:build postgres

package postgres_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestConcurrentSelect fires 1000 goroutines × 100 queries against the shared
// *sql.DB and asserts every one returns the expected integer. Uses -short
// gating because 100k round-trips to a local pg can still take several
// seconds.
func TestConcurrentSelect(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping heavy concurrency test in -short mode")
	}
	db := openDB(t)
	// Raise the pool cap from the harness default since we want real
	// parallelism.
	db.SetMaxOpenConns(32)
	db.SetMaxIdleConns(16)

	const (
		workers = 1000
		each    = 100
	)

	var (
		wg   sync.WaitGroup
		errs atomic.Int64
	)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				var got int
				err := db.QueryRowContext(ctx, "SELECT 42").Scan(&got)
				cancel()
				if err != nil || got != 42 {
					errs.Add(1)
					return
				}
			}
		}()
	}
	wg.Wait()
	if n := errs.Load(); n > 0 {
		t.Fatalf("%d goroutines observed errors or wrong results", n)
	}
}

// TestConcurrentInsert serializes through the pool's per-conn semantics:
// each goroutine owns its own INSERT stream via a unique id.
func TestConcurrentInsert(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping concurrent insert test in -short mode")
	}
	db := openDB(t)
	db.SetMaxOpenConns(16)

	tbl := uniqueTableName(t, "concur")
	createTable(t, db, tbl, "id int primary key, worker int, seq int")

	const workers = 16
	const each = 50

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				id := worker*each + i
				_, err := db.ExecContext(ctx, "INSERT INTO "+tbl+" VALUES ($1,$2,$3)", id, worker, i)
				cancel()
				if err != nil {
					t.Errorf("worker %d seq %d: %v", worker, i, err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var n int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+tbl).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != workers*each {
		t.Fatalf("count = %d, want %d", n, workers*each)
	}
}
