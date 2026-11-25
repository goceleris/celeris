//go:build postgres

package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/postgres"
)

// The native Pool's Rows must tolerate a second Close and a stale handle: see
// driver/postgres/rows_close_idempotent_test.go, whose cases these repeat against a
// real server (extended protocol, binary int8).

func TestPoolRowsCloseTwice(t *testing.T) {
	dsn := dsnFromEnv(t)
	p, err := openPoolEngine(dsn, postgres.WithMaxOpen(2))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// defer + explicit Close.
	err = func() error {
		rows, err := p.QueryContext(ctx, "SELECT $1::bigint", int64(41))
		if err != nil {
			return err
		}
		defer rows.Close()
		var v int64
		if !rows.Next() {
			t.Fatalf("Next=false, Err=%v", rows.Err())
		}
		if err := rows.Scan(&v); err != nil || v != 41 {
			t.Fatalf("Scan = %d, %v", v, err)
		}
		return rows.Close()
	}()
	if err != nil {
		t.Fatalf("explicit Close = %v", err)
	}

	// Methods after Close.
	rows, err := p.QueryContext(ctx, "SELECT 1::bigint")
	if err != nil {
		t.Fatal(err)
	}
	rows.Next()
	_ = rows.Close()
	if err := rows.Close(); err != nil {
		t.Errorf("second Close = %v", err)
	}
	if rows.Next() {
		t.Error("Next after Close = true")
	}
	var v int64
	if err := rows.Scan(&v); err == nil {
		t.Error("Scan after Close = nil error")
	}
	_ = rows.Columns()
	if err := rows.Err(); err != nil {
		t.Errorf("Err after Close = %v", err)
	}

	// Err survives Close.
	er, err := p.QueryContext(ctx, "SELECT 1/(g-2) FROM generate_series(1,3) g")
	if err == nil {
		for er.Next() {
		}
		before := er.Err()
		_ = er.Close()
		if before == nil {
			t.Error("Err before Close = nil, want division by zero")
		}
		if after := er.Err(); after != before {
			t.Errorf("Err after Close = %v, want %v", after, before)
		}
	}

	// Row.Scan twice.
	row := p.QueryRow(ctx, "SELECT 77::bigint")
	if err := row.Scan(&v); err != nil || v != 77 {
		t.Fatalf("Row.Scan = %d, %v", v, err)
	}
	if err := row.Scan(&v); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("second Row.Scan = %v, want sql.ErrNoRows", err)
	}

	if st := p.Stats(); st.InUse != 0 {
		t.Errorf("InUse=%d after every Rows was closed", st.InUse)
	}
}

func TestPoolTxErrTxDone(t *testing.T) {
	dsn := dsnFromEnv(t)
	p, err := openPoolEngine(dsn, postgres.WithMaxOpen(2))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tx, err := p.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); !errors.Is(err, sql.ErrTxDone) {
		t.Errorf("Rollback after Commit = %v, want sql.ErrTxDone", err)
	}
	if err := tx.Commit(); !errors.Is(err, sql.ErrTxDone) {
		t.Errorf("Commit after Commit = %v, want sql.ErrTxDone", err)
	}
	if _, err := tx.ExecContext(ctx, "SELECT 1"); !errors.Is(err, sql.ErrTxDone) {
		t.Errorf("Exec after Commit = %v, want sql.ErrTxDone", err)
	}
}

// TestPoolRowsStaleCloseConcurrent: goroutines that close their Rows twice, with a
// pause between, while workers each query a marker of their own and check their
// rows are open and their conn is theirs. CELERIS_ROWS_QUERIES sets the worker
// queries (default 20000).
func TestPoolRowsStaleCloseConcurrent(t *testing.T) {
	dsn := dsnFromEnv(t)
	total := 20_000
	if testing.Short() {
		total = 2_000
	}
	total = envInt("CELERIS_ROWS_QUERIES", total)
	const workers, closers, maxOpen = 8, 2, 4
	p, err := openPoolEngine(dsn, postgres.WithMaxOpen(maxOpen))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	var bad, stalePanics, staleBadErr atomic.Int64
	var stop atomic.Bool
	var wg, cwg sync.WaitGroup
	for c := 0; c < closers; c++ {
		cwg.Add(1)
		go func(c int) {
			defer cwg.Done()
			for i := 0; !stop.Load(); i++ {
				rows, err := p.QueryContext(ctx, "SELECT $1::bigint", int64(c+1)<<40|int64(i))
				if err != nil {
					return
				}
				_ = rows.Close()
				for k := 0; k < 1+i%4; k++ {
					runtime.Gosched()
				}
				func() {
					defer func() {
						if recover() != nil {
							stalePanics.Add(1)
						}
					}()
					if rows.Close() != nil {
						staleBadErr.Add(1)
					}
				}()
			}
		}(c)
	}
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < total/workers; i++ {
				m := int64(w+1)<<32 | int64(i)
				rows, err := p.QueryContext(ctx, "SELECT $1::bigint", m)
				if err != nil {
					bad.Add(1)
					t.Errorf("worker %d query %d: %v", w, i, err)
					return
				}
				runtime.Gosched()
				var got int64
				if !rows.Next() {
					bad.Add(1)
					t.Errorf("worker %d query %d: rows closed under the worker (Err=%v)", w, i, rows.Err())
					_ = rows.Close()
					continue
				}
				if err := rows.Scan(&got); err != nil || got != m {
					bad.Add(1)
					t.Errorf("worker %d query %d: got %d, %v, want %d", w, i, got, err, m)
				}
				_ = rows.Close()
			}
		}(w)
	}
	wg.Wait()
	stop.Store(true)
	cwg.Wait()
	t.Logf("workers %d queries, %d bad; stale Close panics %d, errors %s", total/workers*workers, bad.Load(), stalePanics.Load(), strconv.FormatInt(staleBadErr.Load(), 10))
	if stalePanics.Load() != 0 || staleBadErr.Load() != 0 || bad.Load() != 0 {
		t.Fail()
	}
	if st := p.Stats(); st.InUse != 0 || st.Idle != st.Open {
		t.Errorf("pool after the run: %+v", st)
	}
}
