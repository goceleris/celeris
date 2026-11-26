//go:build postgres

package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	postgres "github.com/goceleris/celeris/driver/postgres"
)

// A QueryContext with a deadline over a large result is the most ordinary thing
// a database/sql user does. When the deadline expired after the driver had
// promoted the reply to a stream (64 rows buffered) and the caller's select
// took ctx.Done over the streaming-ready signal, the caller waited for the
// request to finish while the event-loop worker sat in a blocking send on the
// full row channel that nobody drained: the worker could not read the server's
// cancel ErrorResponse, so the request never finished, and every other
// connection registered on that worker stalled with it (waitForQueryRows,
// driver/postgres/conn.go).
//
// The tests drive that on a real server through both entries to the event loop
// (database/sql over a Connector, and the native Pool opened WithEngine on a
// server that is not started, which falls back to the driver's own loop) and
// assert, per entry:
//
//  1. nothing hangs: every call runs under a watchdog, so a regression fails the
//     test (with the driver's goroutines in the log) and never hangs CI;
//  2. the worker is free again: connections of the other pool on every worker
//     keep answering SELECTs with per-call markers WHILE the cancelled queries
//     run, and none waits more than a few seconds;
//  3. the connection after a cancelled query is clean: the next query on it
//     returns its own rows (per-query markers, 200 rows, streamed) and none of
//     the buffered rows of the cancelled one (the connection is reused or has
//     been replaced; the test counts which);
//  4. how long a cancelled call takes to return after its deadline is measured
//     and logged (p50, p90, p99, max).
//
// CELERIS_PGHANG_ITERS sets the iterations per entry (default 50, which fails
// on the unfixed driver within the first 20 in every run made; the
// acceptance runs used 2000), CELERIS_PGHANG_ROWS the rows of the big query
// (default 300000) and CELERIS_PGHANG_SEED the seed of the deadlines.

const (
	hangVictimTimeout = 45 * time.Second // a call that outlives this is a hang (wait()'s cancel bound is 30 s)
	hangStallLimit    = 5 * time.Second  // a bystander call that outlives this saw a stuck worker
)

// hangServer is the engine of the native Pools: the server of the run's
// CELERIS_CONFORMANCE_ENGINE mode when it sets one, else a server that is not
// started (the Pool then runs on the driver's own event loop).
func hangServer() postgres.ServerProvider {
	if sp := engineProvider(); sp != nil {
		return sp
	}
	return celeris.New(celeris.Config{Addr: "127.0.0.1:0"})
}

func hangEnvInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil && v > 0 {
		return v
	}
	return def
}

// hangVictim is one entry to the event loop: run executes the big query under
// ctx, reads up to 10 rows and closes; verify runs the marker query.
type hangVictim interface {
	run(ctx context.Context, query string) (returned time.Time, qErr error)
	verify(ctx context.Context, marker string, simple bool) (pid int64, err error)
	close()
}

const verifyRows = 200

func verifyQuery(simple bool, marker string) (string, []any) {
	if simple {
		return fmt.Sprintf("SELECT '%s-' || g::text, pg_backend_pid(), repeat('y', 40) FROM generate_series(1, %d) g", marker, verifyRows), nil
	}
	return fmt.Sprintf("SELECT $1::text || '-' || g::text, pg_backend_pid(), repeat('y', 40) FROM generate_series(1, %d) g", verifyRows), []any{marker}
}

type sqlHangVictim struct{ db *sql.DB }

func (v sqlHangVictim) run(ctx context.Context, query string) (time.Time, error) {
	rows, err := v.db.QueryContext(ctx, query)
	returned := time.Now()
	if err != nil {
		return returned, err
	}
	for n := 0; n < 10 && rows.Next(); n++ { //revive:disable-line:empty-block read ten rows
	}
	_ = rows.Close()
	return returned, nil
}

func (v sqlHangVictim) verify(ctx context.Context, marker string, simple bool) (int64, error) {
	q, args := verifyQuery(simple, marker)
	rows, err := v.db.QueryContext(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	defer func() { _ = rows.Close() }()
	var pid int64
	n := 0
	for rows.Next() {
		var s, pad string
		var p int64
		if err := rows.Scan(&s, &p, &pad); err != nil {
			return 0, err
		}
		n++
		if want := fmt.Sprintf("%s-%d", marker, n); s != want {
			return 0, fmt.Errorf("row %d is %q, want %q: a row of another query surfaced", n, s, want)
		}
		pid = p
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if n != verifyRows {
		return 0, fmt.Errorf("%d rows, want %d", n, verifyRows)
	}
	return pid, nil
}

func (v sqlHangVictim) close() { _ = v.db.Close() }

type poolHangVictim struct{ pool *postgres.Pool }

func (v poolHangVictim) run(ctx context.Context, query string) (time.Time, error) {
	rows, err := v.pool.QueryContext(ctx, query)
	returned := time.Now()
	if err != nil {
		return returned, err
	}
	for n := 0; n < 10 && rows.Next(); n++ { //revive:disable-line:empty-block read ten rows
	}
	_ = rows.Close()
	return returned, nil
}

func (v poolHangVictim) verify(ctx context.Context, marker string, simple bool) (int64, error) {
	q, args := verifyQuery(simple, marker)
	rows, err := v.pool.QueryContext(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	defer func() { _ = rows.Close() }()
	var pid int64
	n := 0
	for rows.Next() {
		var s, pad string
		var p int64
		if err := rows.Scan(&s, &p, &pad); err != nil {
			return 0, err
		}
		n++
		if want := fmt.Sprintf("%s-%d", marker, n); s != want {
			return 0, fmt.Errorf("row %d is %q, want %q: a row of another query surfaced", n, s, want)
		}
		pid = p
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if n != verifyRows {
		return 0, fmt.Errorf("%d rows, want %d", n, verifyRows)
	}
	return pid, nil
}

func (v poolHangVictim) close() { _ = v.pool.Close() }

// hangBystanders runs one SELECT loop per worker of the event loop on its own
// pool (so none shares a connection with the victim), each call with its own
// marker, and notes when a call has been waiting for too long.
type hangBystanders struct {
	pool     *postgres.Pool
	stop     chan struct{}
	wg       sync.WaitGroup
	inflight []atomic.Int64 // unix nanos of the call in flight, 0 when none
	calls    atomic.Int64
	maxNanos atomic.Int64
	mu       sync.Mutex
	errs     []string
}

func startHangBystanders(t *testing.T, dsn string, sp postgres.ServerProvider) *hangBystanders {
	t.Helper()
	// sp is never nil: a Pool opened without WithEngine dials through Go's
	// netpoll (direct mode), not on the event loop the victim's connection is
	// registered on. WithEngine on a server that is not started takes the
	// driver's own loop, the one a Connector uses.
	pool, err := postgres.Open(dsn, postgres.WithEngine(sp), postgres.WithMaxOpen(64))
	if err != nil {
		t.Fatalf("bystander pool: %v", err)
	}
	nw := len(pool.Stats().PerWorker)
	if nw == 0 {
		t.Fatalf("bystander pool has no worker")
	}
	b := &hangBystanders{pool: pool, stop: make(chan struct{}), inflight: make([]atomic.Int64, nw)}
	// One connection per worker slot: all of them at once, so that each dials
	// on its own worker instead of taking the idle one of another.
	var warm sync.WaitGroup
	warmErrs := make([]error, nw)
	for w := range nw {
		warm.Add(1)
		go func() {
			defer warm.Done()
			ctx, cancel := context.WithTimeout(postgres.WithWorker(context.Background(), w), 20*time.Second)
			defer cancel()
			var s string
			warmErrs[w] = pool.QueryRow(ctx, "SELECT pg_sleep(0.3)::text").Scan(&s)
		}()
	}
	warm.Wait()
	for w, err := range warmErrs {
		if err != nil {
			t.Fatalf("bystander warm-up on worker %d: %v", w, err)
		}
	}
	have := map[int]bool{}
	for _, w := range pool.IdleConnWorkers() {
		have[w] = true
	}
	if len(have) != nw {
		t.Fatalf("the bystanders cover %d of %d workers (idle connections by worker %v): the check that a connection shares the victim's worker needs every worker", len(have), nw, pool.IdleConnWorkers())
	}
	for w := 0; w < nw; w++ {
		b.wg.Add(1)
		go b.loop(w)
	}
	return b
}

func (b *hangBystanders) fail(format string, a ...any) {
	b.mu.Lock()
	if len(b.errs) < 8 {
		b.errs = append(b.errs, fmt.Sprintf(format, a...))
	}
	b.mu.Unlock()
}

func (b *hangBystanders) loop(w int) {
	defer b.wg.Done()
	for i := 0; ; i++ {
		select {
		case <-b.stop:
			return
		default:
		}
		marker := fmt.Sprintf("b%d-%d", w, i)
		ctx, cancel := context.WithTimeout(postgres.WithWorker(context.Background(), w), 60*time.Second)
		start := time.Now()
		b.inflight[w].Store(start.UnixNano())
		var got string
		err := b.pool.QueryRow(ctx, "SELECT $1::text", marker).Scan(&got)
		b.inflight[w].Store(0)
		cancel()
		lat := time.Since(start)
		for {
			old := b.maxNanos.Load()
			if int64(lat) <= old || b.maxNanos.CompareAndSwap(old, int64(lat)) {
				break
			}
		}
		b.calls.Add(1)
		switch {
		case err != nil:
			b.fail("worker %d call %d: %v", w, i, err)
		case got != marker:
			b.fail("worker %d call %d: got %q, want %q", w, i, got, marker)
		}
		select {
		case <-b.stop:
			return
		case <-time.After(2 * time.Millisecond):
		}
	}
}

// stalled returns a description of a call that has been in flight for too long,
// "" when none has.
func (b *hangBystanders) stalled() string {
	now := time.Now().UnixNano()
	for w := range b.inflight {
		if s := b.inflight[w].Load(); s != 0 && time.Duration(now-s) > hangStallLimit {
			return fmt.Sprintf("a SELECT on worker %d has been waiting %v", w, time.Duration(now-s).Round(time.Millisecond))
		}
	}
	return ""
}

func (b *hangBystanders) finish(t *testing.T, hung bool) {
	t.Helper()
	close(b.stop)
	if !hung {
		b.wg.Wait()
	}
	b.mu.Lock()
	errs := b.errs
	b.mu.Unlock()
	t.Logf("bystanders: %d SELECTs on %d workers while the cancelled queries ran, slowest %v", b.calls.Load(), len(b.inflight), time.Duration(b.maxNanos.Load()).Round(time.Microsecond))
	for _, e := range errs {
		t.Errorf("bystander: %s", e)
	}
	if d := time.Duration(b.maxNanos.Load()); d > hangStallLimit {
		t.Errorf("the slowest bystander SELECT took %v (limit %v): the worker was held by a cancelled streaming query", d, hangStallLimit)
	}
	if !hung {
		closeBounded(func() { _ = b.pool.Close() })
	}
}

func closeBounded(f func()) {
	done := make(chan struct{})
	go func() { defer close(done); f() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
	}
}

func hangGoroutines() string {
	buf := make([]byte, 4<<20)
	buf = buf[:runtime.Stack(buf, true)]
	var keep []string
	for _, g := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(g, "driver/postgres") || strings.Contains(g, "driver/internal/eventloop") {
			keep = append(keep, g)
		}
	}
	s := strings.Join(keep, "\n\n")
	if len(s) > 24<<10 {
		s = s[:24<<10] + "\n...(truncated)"
	}
	return s
}

func quantile(d []time.Duration, q float64) time.Duration {
	if len(d) == 0 {
		return 0
	}
	i := int(q * float64(len(d)-1))
	return d[i]
}

func runCancelledStreamHang(t *testing.T, entry string, v hangVictim, by *hangBystanders) {
	iters := hangEnvInt("CELERIS_PGHANG_ITERS", 50)
	rows := hangEnvInt("CELERIS_PGHANG_ROWS", 300000)
	seed := uint64(hangEnvInt("CELERIS_PGHANG_SEED", 1))
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	query := fmt.Sprintf("SELECT g, repeat('x', 200) FROM generate_series(1, %d) g", rows)

	hung := false
	defer func() {
		by.finish(t, hung)
		if !hung {
			closeBounded(v.close)
		}
	}()

	type result struct {
		qErr     error
		returned time.Time
		pid      int64
		vErr     error
	}
	var (
		overruns             []time.Duration
		totals               []time.Duration
		nOK, nCtx, nPG, nBad int
		reused, replaced     int
		lastPID              int64
	)
	for i := 0; i < iters; i++ {
		d := time.Duration(200+rng.IntN(14800)) * time.Microsecond
		marker := fmt.Sprintf("%s%d", entry, i)
		simple := i%2 == 0
		resCh := make(chan result, 1)
		start := time.Now()
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), d)
			defer cancel()
			var r result
			r.returned, r.qErr = v.run(ctx, query)
			vctx, vcancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer vcancel()
			r.pid, r.vErr = v.verify(vctx, marker, simple)
			resCh <- r
		}()
		var r result
		tick := time.NewTicker(100 * time.Millisecond)
	wait:
		for {
			select {
			case r = <-resCh:
				break wait
			case <-tick.C:
				if s := by.stalled(); s != "" {
					hung = true
					tick.Stop()
					t.Fatalf("iteration %d (deadline %v): %s while the cancelled query is in progress: the worker is stuck\n%s", i, d, s, hangGoroutines())
				}
				if time.Since(start) > hangVictimTimeout {
					hung = true
					tick.Stop()
					t.Fatalf("iteration %d (deadline %v): the cancelled query did not return in %v: a hang\n%s", i, d, hangVictimTimeout, hangGoroutines())
				}
			}
		}
		tick.Stop()
		totals = append(totals, time.Since(start))
		if o := r.returned.Sub(start.Add(d)); r.qErr != nil && o > time.Second {
			t.Logf("slow: iteration %d (deadline %v): the call returned %v after it (%v), the whole iteration took %v", i, d, o.Round(time.Millisecond), r.qErr, time.Since(start).Round(time.Millisecond))
		}

		switch {
		case r.qErr == nil:
			nOK++
		case errors.Is(r.qErr, context.DeadlineExceeded) || errors.Is(r.qErr, context.Canceled):
			nCtx++
			if o := r.returned.Sub(start.Add(d)); o > 0 {
				overruns = append(overruns, o)
			} else {
				overruns = append(overruns, 0)
			}
		case strings.Contains(r.qErr.Error(), "57014") || strings.Contains(r.qErr.Error(), "canceling statement"):
			nPG++
			overruns = append(overruns, max(0, r.returned.Sub(start.Add(d))))
		default:
			nBad++
			if nBad <= 3 {
				t.Errorf("iteration %d (deadline %v): unexpected error %v", i, d, r.qErr)
			}
		}
		if r.vErr != nil {
			t.Fatalf("iteration %d (deadline %v, qErr %v): the query after the cancelled one: %v", i, d, r.qErr, r.vErr)
		}
		if lastPID != 0 {
			if r.pid == lastPID {
				reused++
			} else {
				replaced++
			}
		}
		lastPID = r.pid
	}

	slices.Sort(overruns)
	slices.Sort(totals)
	t.Logf("PGHANG %s iterations=%d rows=%d seed=%d: query returned rows %d, ctx error %d, server cancel error %d, other %d; connection after: reused %d, replaced %d", entry, iters, rows, seed, nOK, nCtx, nPG, nBad, reused, replaced)
	t.Logf("PGHANG %s cancelled-call return after the deadline (n=%d): p50 %v p90 %v p99 %v max %v", entry, len(overruns),
		quantile(overruns, 0.5).Round(time.Microsecond), quantile(overruns, 0.9).Round(time.Microsecond),
		quantile(overruns, 0.99).Round(time.Microsecond), quantile(overruns, 1).Round(time.Microsecond))
	t.Logf("PGHANG %s whole iteration (query, 10 rows, close, verify) (n=%d): p50 %v p90 %v p99 %v max %v", entry, len(totals),
		quantile(totals, 0.5).Round(time.Microsecond), quantile(totals, 0.9).Round(time.Microsecond),
		quantile(totals, 0.99).Round(time.Microsecond), quantile(totals, 1).Round(time.Microsecond))
	if nBad > 0 {
		t.Errorf("%d iterations returned an error that is neither the context's nor the server's cancel", nBad)
	}
	if m := quantile(overruns, 1); m > 35*time.Second {
		t.Errorf("a cancelled call returned %v after its deadline: more than the 30 s bound", m)
	}
}

// TestCancelledStreamingQueryDatabaseSQL: database/sql over a Connector, the
// entry the report came from.
func TestCancelledStreamingQueryDatabaseSQL(t *testing.T) {
	dsn := dsnFromEnv(t)
	db, err := openSQLEngine(dsn)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	by := startHangBystanders(t, dsn, hangServer())
	runCancelledStreamHang(t, "sql", sqlHangVictim{db}, by)
}

// TestCancelledStreamingQueryPool: the native Pool in loop mode, WithEngine on
// a server that is not started (the driver then runs its own loop), unless the
// run selects an engine with CELERIS_CONFORMANCE_ENGINE.
func TestCancelledStreamingQueryPool(t *testing.T) {
	dsn := dsnFromEnv(t)
	sp := hangServer()
	pool, err := postgres.Open(dsn, postgres.WithEngine(sp), postgres.WithMaxOpen(1))
	if err != nil {
		t.Fatal(err)
	}
	by := startHangBystanders(t, dsn, sp)
	runCancelledStreamHang(t, "pool", poolHangVictim{pool}, by)
}
