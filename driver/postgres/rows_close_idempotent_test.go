package postgres

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/driver/postgres/protocol"
)

// The native Pool's Rows (and Row) must tolerate being used after Close: a second
// Close is a no-op that returns nil, and a handle the caller still holds can never
// act on another query's rows or connection. Before the fix Rows.Close zeroed the
// wrapper and put it back in a sync.Pool: `defer rows.Close()` plus an explicit
// Close nil-dereferenced, and a stale Close could close the rows (and release the
// conn) of whoever had taken the wrapper from the pool in the meantime.

// echoPGServer is a fake backend for the simple query protocol: "SELECT <n>"
// answers one row holding n (int8, text format); BEGIN / COMMIT / ROLLBACK answer
// their command tag; anything else answers an empty SELECT 0.
func echoPGServer(tb testing.TB) func(net.Conn) {
	return func(c net.Conn) {
		fakePGTrustStartupB(tb, c, 1, 2, func(c net.Conn) {
			var hdr [5]byte
			body := make([]byte, 256)
			var out []byte
			for {
				if _, err := io.ReadFull(c, hdr[:]); err != nil {
					return
				}
				n := int(binary.BigEndian.Uint32(hdr[1:5])) - 4
				if n < 0 {
					return
				}
				if n > len(body) {
					body = make([]byte, n)
				}
				if _, err := io.ReadFull(c, body[:n]); err != nil {
					return
				}
				switch hdr[0] {
				case protocol.MsgTerminate:
					return
				case protocol.MsgQuery:
				default:
					continue
				}
				q := strings.TrimRight(string(body[:n]), "\x00")
				out = out[:0]
				status := byte('I')
				switch {
				case strings.HasPrefix(q, "SELECT ") && q != "SELECT err":
					v := strings.TrimPrefix(q, "SELECT ")
					rd := buildRowDescription([]colSpec{{name: "n", typeOID: protocol.OIDInt8, typeSize: 8, format: protocol.FormatText}})
					out = append(out, pgMsg(protocol.BackendRowDescription, rd)...)
					out = append(out, pgMsg(protocol.BackendDataRow, buildDataRow([][]byte{[]byte(v)}))...)
					out = append(out, pgMsg(protocol.BackendCommandComplete, append([]byte("SELECT 1"), 0))...)
				case q == "SELECT err":
					// One row, then a server error: the rows' Err() is that error.
					rd := buildRowDescription([]colSpec{{name: "n", typeOID: protocol.OIDInt8, typeSize: 8, format: protocol.FormatText}})
					out = append(out, pgMsg(protocol.BackendRowDescription, rd)...)
					out = append(out, pgMsg(protocol.BackendDataRow, buildDataRow([][]byte{[]byte("1")}))...)
					out = append(out, pgMsg(protocol.BackendErrorResponse, []byte("SERROR\x00C22012\x00Mdivision by zero\x00\x00"))...)
				case q == "BEGIN":
					status = 'T'
					out = append(out, pgMsg(protocol.BackendCommandComplete, append([]byte("BEGIN"), 0))...)
				case q == "COMMIT" || q == "ROLLBACK":
					out = append(out, pgMsg(protocol.BackendCommandComplete, append([]byte(q), 0))...)
				default:
					out = append(out, pgMsg(protocol.BackendCommandComplete, append([]byte("SELECT 0"), 0))...)
				}
				out = append(out, pgMsg(protocol.BackendReadyForQuery, []byte{status})...)
				if _, err := c.Write(out); err != nil {
					return
				}
			}
		})
	}
}

// tctx bounds a test: on a tree where a leaked conn exhausts the pool, an
// acquire must fail the test, not hang it.
func tctx(t testing.TB) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func openEchoPool(t testing.TB, maxOpen int) *Pool {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	h := echoPGServer(t)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go h(c)
		}
	}()
	host, port, _ := net.SplitHostPort(ln.Addr().String())
	p, err := Open("postgres://u@"+host+":"+port+"/d?sslmode=disable", WithMaxOpen(maxOpen))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// noPanic runs f and reports a panic as a test error instead of killing the run.
func noPanic(t *testing.T, what string, f func()) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("%s: panic: %v", what, r)
		}
	}()
	f()
}

func scanMarker(rows *Rows) (int64, error) {
	var v int64
	if !rows.Next() {
		return 0, fmt.Errorf("Next=false, Err=%v", rows.Err())
	}
	return v, rows.Scan(&v)
}

func TestRowsCloseTwice(t *testing.T) {
	p := openEchoPool(t, 2)
	ctx := tctx(t)

	t.Run("defer_and_explicit", func(t *testing.T) {
		noPanic(t, "defer+explicit Close", func() {
			err := func() error {
				rows, err := p.QueryContext(ctx, "SELECT 41")
				if err != nil {
					return err
				}
				defer rows.Close() //nolint:errcheck // the idiom under test: a deferred Close next to an explicit one
				if v, err := scanMarker(rows); err != nil || v != 41 {
					return fmt.Errorf("scan: %d, %v", v, err)
				}
				return rows.Close()
			}()
			if err != nil {
				t.Errorf("explicit Close: %v", err)
			}
		})
	})

	t.Run("second_and_third_return_nil", func(t *testing.T) {
		rows, err := p.QueryContext(ctx, "SELECT 1")
		if err != nil {
			t.Fatal(err)
		}
		if err := rows.Close(); err != nil {
			t.Fatalf("first Close: %v", err)
		}
		for i := 2; i <= 3; i++ {
			noPanic(t, fmt.Sprintf("Close #%d", i), func() {
				if err := rows.Close(); err != nil {
					t.Errorf("Close #%d = %v, want nil", i, err)
				}
			})
		}
	})

	t.Run("methods_after_close", func(t *testing.T) {
		rows, err := p.QueryContext(ctx, "SELECT 5")
		if err != nil {
			t.Fatal(err)
		}
		if !rows.Next() {
			t.Fatalf("Next=false, Err=%v", rows.Err())
		}
		_ = rows.Close()
		noPanic(t, "Next after Close", func() {
			if rows.Next() {
				t.Error("Next after Close = true")
			}
		})
		noPanic(t, "Scan after Close", func() {
			var v int64
			if err := rows.Scan(&v); err == nil {
				t.Errorf("Scan after Close = nil error (v=%d), want an error", v)
			}
		})
		noPanic(t, "Err after Close", func() {
			if err := rows.Err(); err != nil {
				t.Errorf("Err after Close = %v, want nil", err)
			}
		})
		noPanic(t, "Columns after Close", func() { _ = rows.Columns() })
		noPanic(t, "Close after all", func() {
			if err := rows.Close(); err != nil {
				t.Errorf("Close = %v", err)
			}
		})
	})

	t.Run("scan_after_close_never_reads_recycled_bytes", func(t *testing.T) {
		// Scan after Close must not decode from the released request slab: a later
		// query reuses it.
		for i := 0; i < 200; i++ {
			r1, err := p.QueryContext(ctx, "SELECT 111")
			if err != nil {
				t.Fatal(err)
			}
			if !r1.Next() {
				t.Fatalf("Next=false, Err=%v", r1.Err())
			}
			_ = r1.Close()
			r2, err := p.QueryContext(ctx, "SELECT 222")
			if err != nil {
				t.Fatal(err)
			}
			noPanic(t, "stale Scan", func() {
				var v int64
				if err := r1.Scan(&v); err == nil {
					t.Errorf("stale Scan succeeded with %d", v)
				}
			})
			if v, err := scanMarker(r2); err != nil || v != 222 {
				t.Fatalf("r2 = %d, %v, want 222", v, err)
			}
			_ = r2.Close()
		}
	})

	t.Run("tx_rows", func(t *testing.T) {
		tx, err := p.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		rows, err := tx.QueryContext(ctx, "SELECT 9")
		if err != nil {
			t.Fatal(err)
		}
		if v, err := scanMarker(rows); err != nil || v != 9 {
			t.Fatalf("scan: %d, %v", v, err)
		}
		_ = rows.Close()
		noPanic(t, "tx rows second Close", func() {
			if err := rows.Close(); err != nil {
				t.Errorf("second Close = %v", err)
			}
		})
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit: %v", err)
		}
	})
}

func TestRowScanTwice(t *testing.T) {
	p := openEchoPool(t, 2)
	ctx := tctx(t)

	t.Run("pool", func(t *testing.T) {
		row := p.QueryRow(ctx, "SELECT 77")
		var v int64
		noPanic(t, "first Row.Scan", func() {
			if err := row.Scan(&v); err != nil || v != 77 {
				t.Errorf("first Scan = %d, %v", v, err)
			}
		})
		noPanic(t, "second Row.Scan", func() {
			var w int64
			if err := row.Scan(&w); !errors.Is(err, sql.ErrNoRows) {
				t.Errorf("second Scan = %v (w=%d), want sql.ErrNoRows", err, w)
			}
		})
		noPanic(t, "Row.Err", func() { _ = row.Err() })
	})

	t.Run("pool_never_reads_another_queries_row", func(t *testing.T) {
		for i := 0; i < 200; i++ {
			row := p.QueryRow(ctx, "SELECT 1")
			var v int64
			if err := row.Scan(&v); err != nil {
				t.Fatal(err)
			}
			other, err := p.QueryContext(ctx, "SELECT 2")
			if err != nil {
				t.Fatal(err)
			}
			noPanic(t, "second Row.Scan", func() {
				var w int64
				if err := row.Scan(&w); !errors.Is(err, sql.ErrNoRows) {
					t.Errorf("second Scan = %v (w=%d), want sql.ErrNoRows", err, w)
				}
			})
			if v, err := scanMarker(other); err != nil || v != 2 {
				t.Fatalf("other = %d, %v, want 2", v, err)
			}
			_ = other.Close()
		}
	})

	t.Run("tx", func(t *testing.T) {
		tx, err := p.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		row := tx.QueryRow(ctx, "SELECT 3")
		var v int64
		if err := row.Scan(&v); err != nil || v != 3 {
			t.Fatalf("Scan = %d, %v", v, err)
		}
		noPanic(t, "second tx Row.Scan", func() {
			var w int64
			if err := row.Scan(&w); !errors.Is(err, sql.ErrNoRows) {
				t.Errorf("second Scan = %v (w=%d), want sql.ErrNoRows", err, w)
			}
		})
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("failed_query_row_scan_twice", func(t *testing.T) {
		pc, cancel := context.WithCancel(ctx)
		cancel()
		row := p.QueryRow(pc, "SELECT 1")
		var v int64
		for i := 0; i < 2; i++ {
			noPanic(t, "Row.Scan of a failed query", func() {
				if err := row.Scan(&v); err == nil {
					t.Error("Scan of a failed query = nil error")
				}
			})
		}
	})

	t.Run("no_rows_sentinel_is_kept", func(t *testing.T) {
		row := p.QueryRow(ctx, "VALUES (1)") // the echo backend answers no row
		var v int64
		if err := row.Scan(&v); err != sql.ErrNoRows {
			t.Errorf("Scan of an empty result = %v, want sql.ErrNoRows", err)
		}
	})
}

// TestRowsStaleHandleDoesNotHitNextQuery is the deterministic form of the
// cross-request release: r1 is closed, r2 is the next Query on the same
// goroutine (a sync.Pool hands a Put wrapper straight back to the same P) and
// the stale r1.Close must leave r2 open and its conn held.
func TestRowsStaleHandleDoesNotHitNextQuery(t *testing.T) {
	p := openEchoPool(t, 1) // one conn: a stale release is visible at once
	ctx := tctx(t)
	aliased := 0
	for i := 0; i < 3000; i++ {
		r1, err := p.QueryContext(ctx, "SELECT 1")
		if err != nil {
			t.Fatal(err)
		}
		if err := r1.Close(); err != nil {
			t.Fatal(err)
		}
		r2, err := p.QueryContext(ctx, "SELECT 2")
		if err != nil {
			t.Fatal(err)
		}
		if r1 == r2 {
			aliased++
		}
		noPanic(t, "stale Close", func() {
			if err := r1.Close(); err != nil {
				t.Errorf("stale Close = %v", err)
			}
		})
		if st := p.Stats(); st.InUse != 1 {
			t.Fatalf("iteration %d (aliased=%v): after a stale Close InUse=%d, want 1 (r2 still holds the conn)", i, r1 == r2, st.InUse)
		}
		v, err := scanMarker(r2)
		if err != nil || v != 2 {
			t.Fatalf("iteration %d (aliased=%v): r2 = %d, %v, want 2: the stale Close hit r2", i, r1 == r2, v, err)
		}
		_ = r2.Close()
	}
	t.Logf("wrapper handed to the next Query: %d of 3000 iterations", aliased)
	if aliased != 0 {
		t.Errorf("a Rows wrapper was handed out again while the caller could still hold it (%d times)", aliased)
	}
}

// TestRowsStaleCloseConcurrent runs a goroutine that closes its Rows twice, with a
// pause between so the wrapper could have been reused, against workers that each
// query a marker of their own and check their rows are still open and their conn
// is still theirs after a yield. ROWSFIX_QUERIES sets the total worker queries.
func TestRowsStaleCloseConcurrent(t *testing.T) {
	total := 50_000
	if testing.Short() {
		total = 4_000
	}
	if s := os.Getenv("ROWSFIX_QUERIES"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n <= 0 {
			t.Fatalf("ROWSFIX_QUERIES=%q", s)
		}
		total = n
	}
	const workers, closers, maxOpen = 8, 2, 4 // maxOpen is the pool's MaxOpen
	p := openEchoPool(t, maxOpen)
	ctx := tctx(t)

	var (
		bad          atomic.Int64 // worker saw its rows closed / another's value
		stalePanics  atomic.Int64 // stale Close panicked (wrapper not yet reused)
		staleReused  atomic.Int64 // stale Close returned without panic: the wrapper was live again
		staleBadErr  atomic.Int64
		staleQueries atomic.Int64
		stop         atomic.Bool
		logged       atomic.Int32
	)
	report := func(format string, a ...any) {
		if logged.Add(1) <= 8 {
			t.Errorf(format, a...)
		}
	}

	var wg, cwg sync.WaitGroup
	for c := 0; c < closers; c++ {
		cwg.Add(1)
		go func(c int) {
			defer cwg.Done()
			for i := 0; !stop.Load(); i++ {
				m := int64(1_000_000_000+c)<<20 | int64(i&0xfffff)
				rows, err := p.QueryContext(ctx, "SELECT "+strconv.FormatInt(m, 10))
				if err != nil {
					report("closer query: %v", err)
					return
				}
				staleQueries.Add(1)
				_ = rows.Close()
				// Pause so that, if the wrapper was recycled, another goroutine has it.
				for k := 0; k < 1+i%4; k++ {
					runtime.Gosched()
				}
				if i%8 == 0 {
					time.Sleep(time.Duration(i%50) * time.Microsecond)
				}
				func() {
					defer func() {
						if r := recover(); r != nil {
							stalePanics.Add(1)
						}
					}()
					if err := rows.Close(); err != nil {
						staleBadErr.Add(1)
					}
					staleReused.Add(1)
				}()
			}
		}(c)
	}
	per := total / workers
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < per; i++ {
				m := int64(w+1)<<32 | int64(i)
				rows, err := p.QueryContext(ctx, "SELECT "+strconv.FormatInt(m, 10))
				if err != nil {
					report("worker %d query %d: %v", w, i, err)
					bad.Add(1)
					return
				}
				// Hold the rows across a yield: a stale Close lands here.
				runtime.Gosched()
				var got int64
				if !rows.Next() {
					bad.Add(1)
					report("worker %d query %d: rows closed under the worker (Err=%v)", w, i, rows.Err())
					_ = rows.Close()
					continue
				}
				if err := rows.Scan(&got); err != nil || got != m {
					bad.Add(1)
					report("worker %d query %d: got %d, %v, want %d", w, i, got, err, m)
				}
				if rows.Next() {
					bad.Add(1)
					report("worker %d query %d: a second row", w, i)
				}
				if err := rows.Close(); err != nil {
					bad.Add(1)
					report("worker %d query %d: Close: %v", w, i, err)
				}
			}
		}(w)
	}
	wg.Wait()
	stop.Store(true)
	cwg.Wait()

	t.Logf("workers: %d queries, %d bad; closers: %d queries, %d stale Close(s) returned, %d panicked, %d with an error",
		per*workers, bad.Load(), staleQueries.Load(), staleReused.Load(), stalePanics.Load(), staleBadErr.Load())
	if n := stalePanics.Load(); n != 0 {
		t.Errorf("%d stale Close(s) panicked", n)
	}
	if n := staleBadErr.Load(); n != 0 {
		t.Errorf("%d stale Close(s) returned an error", n)
	}
	if n := bad.Load(); n != 0 {
		t.Errorf("%d worker queries saw their rows or conn disturbed (a stale Close reached another query)", n)
	}
	st := p.Stats()
	// A double release shows as InUse != 0 or Idle != Open. (Open can briefly pass
	// maxOpen by one on its own: two dials racing for the last slot; main does too.)
	if st.InUse != 0 || st.Idle != st.Open {
		t.Errorf("pool after the run: %+v, want InUse=0 and Idle==Open (a double release corrupts these)", st)
	}
}

// A recovered double Close used to leave the zeroed wrapper in the sync.Pool with
// done=true: the next, unrelated query got empty rows and its conn never came back.
func TestRowsPoisonAfterRecoveredDoubleClose(t *testing.T) {
	p := openEchoPool(t, 2)
	ctx := tctx(t)
	for i := 0; i < 50; i++ {
		r, err := p.QueryContext(ctx, "SELECT 1")
		if err != nil {
			t.Fatal(err)
		}
		_ = r.Close()
		func() {
			defer func() { _ = recover() }() // what a recover middleware does
			_ = r.Close()
		}()
		r2, err := p.QueryContext(ctx, "SELECT 2")
		if err != nil {
			t.Fatalf("iteration %d: the next query: %v", i, err)
		}
		if v, err := scanMarker(r2); err != nil || v != 2 {
			t.Fatalf("iteration %d: the next, unrelated query got %d, %v, want 2 (a poisoned wrapper answers no rows)", i, v, err)
		}
		_ = r2.Close()
		if st := p.Stats(); st.InUse != 0 {
			t.Fatalf("iteration %d: InUse=%d after the next query closed: its conn leaked", i, st.InUse)
		}
	}
}

// Err must survive Close and belong to its own query (it used to come back nil,
// or as the error of whichever query had taken the recycled wrapper).
func TestRowsErrSurvivesClose(t *testing.T) {
	p := openEchoPool(t, 2)
	ctx := tctx(t)
	for i := 0; i < 200; i++ {
		r1, err := p.QueryContext(ctx, "SELECT err")
		if err != nil {
			t.Fatalf("QueryContext(SELECT err): %v", err)
		}
		for r1.Next() {
		}
		before := r1.Err()
		if before == nil {
			t.Fatal("Err before Close = nil, want the server error")
		}
		_ = r1.Close()
		// Another query, which a recycled wrapper would have handed r1's state to.
		r2, err := p.QueryContext(ctx, "SELECT 5")
		if err != nil {
			t.Fatal(err)
		}
		var after, other error
		noPanic(t, "Err after Close", func() { after, other = r1.Err(), r2.Err() })
		if after != before {
			t.Fatalf("iteration %d: Err after Close = %v, want %v", i, after, before)
		}
		if other != nil {
			t.Fatalf("iteration %d: a live query's Err = %v, want nil", i, other)
		}
		_ = r2.Close()
		// A stale Close and the clean query's Err stay separate.
		if err := r1.Close(); err != nil {
			t.Fatalf("second Close = %v", err)
		}
	}
}

func TestTxErrTxDone(t *testing.T) {
	p := openEchoPool(t, 2)
	ctx := tctx(t)
	isDone := func(what string, err error) {
		t.Helper()
		if !errors.Is(err, sql.ErrTxDone) {
			t.Errorf("%s = %v, want errors.Is(err, sql.ErrTxDone)", what, err)
		}
	}
	begin := func() *Tx {
		tx, err := p.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		return tx
	}

	tx := begin()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	isDone("Commit after Commit", tx.Commit())
	isDone("Rollback after Commit", tx.Rollback())
	_, err := tx.ExecContext(ctx, "SELECT 1")
	isDone("Exec after Commit", err)
	_, err = tx.QueryContext(ctx, "SELECT 1")
	isDone("Query after Commit", err)
	isDone("QueryRow.Scan after Commit", tx.QueryRow(ctx, "SELECT 1").Scan(new(int64)))
	isDone("Savepoint after Commit", tx.Savepoint(ctx, "sp"))
	isDone("ReleaseSavepoint after Commit", tx.ReleaseSavepoint(ctx, "sp"))
	isDone("RollbackToSavepoint after Commit", tx.RollbackToSavepoint(ctx, "sp"))

	tx = begin()
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	isDone("Rollback after Rollback", tx.Rollback())
	isDone("Commit after Rollback", tx.Commit())

	// The deferred Rollback of a committed tx must not release the conn twice.
	tx = begin()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	_ = tx.Rollback()
	if st := p.Stats(); st.InUse != 0 || st.Idle != st.Open {
		t.Errorf("pool after Commit + deferred Rollback: %+v", st)
	}
	// The message stays what it was.
	if err := tx.Commit(); err == nil || !strings.Contains(err.Error(), "tx already finished") {
		t.Errorf("message = %v, want it to keep \"tx already finished\"", err)
	}
}

// done is read by a deferred Rollback while another goroutine may be finishing
// the tx: exactly one of the finishers wins, none races (run under -race).
func TestTxFinishRace(t *testing.T) {
	p := openEchoPool(t, 2)
	ctx := tctx(t)
	for i := 0; i < 200; i++ {
		tx, err := p.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var wins atomic.Int32
		for k := 0; k < 4; k++ {
			wg.Add(1)
			go func(k int) {
				defer wg.Done()
				var err error
				if k%2 == 0 {
					err = tx.Commit()
				} else {
					err = tx.Rollback()
				}
				if err == nil {
					wins.Add(1)
				} else if !errors.Is(err, sql.ErrTxDone) {
					t.Errorf("finisher %d: %v", k, err)
				}
			}(k)
		}
		wg.Wait()
		if wins.Load() != 1 {
			t.Fatalf("iteration %d: %d finishers won, want 1", i, wins.Load())
		}
		if st := p.Stats(); st.InUse != 0 || st.Idle != st.Open {
			t.Fatalf("iteration %d: pool %+v after one tx finished 4 times", i, st)
		}
	}
}
