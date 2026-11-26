//go:build postgres

package postgres_test

import (
	"context"
	"fmt"
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

// Pool.CopyFrom with a deadline that expires while the call waits for the
// server's CopyInResponse used to wedge for good: the ctx branch of the wait for the
// response sent a CancelRequest and then waited for the request to finish with
// no bound. The cancel is no way out of a statement the server starts after it
// (or that is not running yet): the server sat in COPY IN waiting for data that
// the returning call would never send, nothing completed the request, and the
// caller (and the pool slot it held) never came back. With 8 goroutines and a
// 20-420 microsecond deadline on half the calls it took a few dozen calls.
//
// The call now closes the connection (its session state is unknown) and
// returns the context's error at once; the pool dials a replacement. This test
// asserts, in both modes of the Pool (direct, no engine; loop, WithEngine on a
// server that is not started):
//
//  1. nothing hangs: a call in flight for more than copyHangLimit fails the
//     test, with the goroutines in the log, instead of hanging CI;
//  2. after a cancelled call the pool serves the next one with its own rows (a
//     COPY with a per-call marker, counted by that marker): the connection is
//     closed and replaced, never left half-way in COPY mode;
//  3. no backend is left in COPY IN on the server once the run is over;
//  4. how long a cancelled call takes to return after its deadline (logged).
//
// CELERIS_COPYWEDGE_ITERS sets the calls per goroutine (default 50, 8
// goroutines; the unfixed driver wedges within the first dozens of calls).

const copyHangLimit = 20 * time.Second

func copyWedgeIters() int {
	if v, err := strconv.Atoi(os.Getenv("CELERIS_COPYWEDGE_ITERS")); err == nil && v > 0 {
		return v
	}
	return 50
}

func TestCopyFromCancelledBeforeServerAnswersDirect(t *testing.T) {
	dsn := dsnFromEnv(t)
	pool, err := postgres.Open(dsn, postgres.WithMaxOpen(8))
	if err != nil {
		t.Fatal(err)
	}
	runCopyWedge(t, dsn, "direct", pool)
}

func TestCopyFromCancelledBeforeServerAnswersLoop(t *testing.T) {
	dsn := dsnFromEnv(t)
	sp := engineProvider()
	if sp == nil {
		sp = celeris.New(celeris.Config{Addr: "127.0.0.1:0"})
	}
	pool, err := postgres.Open(dsn, postgres.WithEngine(sp), postgres.WithMaxOpen(8))
	if err != nil {
		t.Fatal(err)
	}
	runCopyWedge(t, dsn, "loop", pool)
}

func runCopyWedge(t *testing.T, dsn, mode string, pool *postgres.Pool) {
	db := openDB(t)
	table := uniqueTableName(t, "copy_wedge_"+mode)
	createTable(t, db, table, "g bigint, pad text")
	iters := copyWedgeIters()
	const workers = 8
	pad := strings.Repeat("p", 24000)

	hung := false
	defer func() {
		if !hung {
			// Bounded: a pool with a wedged call would wait for it in Close.
			closed := make(chan struct{})
			go func() { defer close(closed); _ = pool.Close() }()
			select {
			case <-closed:
			case <-time.After(10 * time.Second):
			}
		}
	}()

	type workerState struct {
		inflight atomic.Int64 // unix nanos of the call in flight, 0 when none
	}
	states := make([]workerState, workers)
	var (
		mu        sync.Mutex
		overruns  []time.Duration
		cancelled int
		succeeded int
		failures  []string
	)
	fail := func(format string, a ...any) {
		mu.Lock()
		if len(failures) < 10 {
			failures = append(failures, fmt.Sprintf(format, a...))
		}
		mu.Unlock()
	}
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				// The call under test: half of them with a deadline that ends before the
				// server can have answered.
				rows := make([][]any, 2+(w+i)%9)
				marker := int64(w*1_000_000 + i)
				for r := range rows {
					rows[r] = []any{marker, pad}
				}
				tight := i%2 == 0
				d := 5 * time.Second
				if tight {
					d = time.Duration(20+(i*37+w*101)%400) * time.Microsecond
				}
				start := time.Now()
				states[w].inflight.Store(start.UnixNano())
				ctx, cancel := context.WithTimeout(context.Background(), d)
				n, err := pool.CopyFrom(ctx, table, []string{"g", "pad"}, postgres.CopyFromSlice(rows))
				cancel()
				states[w].inflight.Store(0)
				returned := time.Since(start)
				switch {
				case err == nil && n != int64(len(rows)):
					fail("worker %d call %d: %d rows copied, want %d", w, i, n, len(rows))
				case err == nil:
					mu.Lock()
					succeeded++
					mu.Unlock()
				case tight:
					mu.Lock()
					cancelled++
					overruns = append(overruns, max(0, returned-d))
					mu.Unlock()
				default:
					fail("worker %d call %d (5 s deadline): %v", w, i, err)
				}

				// The next call has to find a clean connection (or a replacement): a COPY
				// with its own marker is counted by that marker.
				next := int64(-1 - (w*1_000_000 + i))
				nrows := make([][]any, 3)
				for r := range nrows {
					nrows[r] = []any{next, "next"}
				}
				states[w].inflight.Store(time.Now().UnixNano())
				nctx, ncancel := context.WithTimeout(context.Background(), 15*time.Second)
				n2, err := pool.CopyFrom(nctx, table, []string{"g", "pad"}, postgres.CopyFromSlice(nrows))
				var got int64
				if err == nil {
					err = pool.QueryRow(nctx, fmt.Sprintf("SELECT count(*) FROM %s WHERE g = $1", table), next).Scan(&got)
				}
				ncancel()
				states[w].inflight.Store(0)
				switch {
				case err != nil:
					fail("worker %d call %d: the COPY after the cancelled one: %v", w, i, err)
				case n2 != 3 || got != 3:
					fail("worker %d call %d: the COPY after the cancelled one copied %d, the table holds %d rows of its marker, want 3", w, i, n2, got)
				}
			}
		}()
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
wait:
	for {
		select {
		case <-done:
			break wait
		case <-tick.C:
			now := time.Now().UnixNano()
			for w := range states {
				if s := states[w].inflight.Load(); s != 0 && time.Duration(now-s) > copyHangLimit {
					hung = true
					t.Fatalf("worker %d: a CopyFrom has been in flight for %v: wedged\n%s", w, time.Duration(now-s).Round(time.Millisecond), copyWedgeGoroutines())
				}
			}
		}
	}

	mu.Lock()
	slices.Sort(overruns)
	t.Logf("COPYWEDGE %s: %d calls per worker x %d workers: %d succeeded, %d cancelled with the context's error", mode, iters, workers, succeeded, cancelled)
	t.Logf("COPYWEDGE %s cancelled call return after the deadline (n=%d): p50 %v p90 %v p99 %v max %v", mode, len(overruns),
		copyQuantile(overruns, 0.5).Round(time.Microsecond), copyQuantile(overruns, 0.9).Round(time.Microsecond),
		copyQuantile(overruns, 0.99).Round(time.Microsecond), copyQuantile(overruns, 1).Round(time.Microsecond))
	for _, f := range failures {
		t.Errorf("%s", f)
	}
	mu.Unlock()

	// No backend left in COPY IN: closed connections end their COPY on the server.
	deadline := time.Now().Add(10 * time.Second)
	for {
		var stuck int
		q := fmt.Sprintf("SELECT count(*) FROM pg_stat_activity WHERE query LIKE 'COPY %%%s%%' AND state <> 'idle'", table)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := db.QueryRowContext(ctx, q).Scan(&stuck)
		cancel()
		if err != nil {
			t.Errorf("pg_stat_activity: %v", err)
			return
		}
		if stuck == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("%d backends are still in COPY on %s 10 s after the run: connections left half-way in COPY IN", stuck, table)
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func copyQuantile(d []time.Duration, q float64) time.Duration {
	if len(d) == 0 {
		return 0
	}
	return d[int(q*float64(len(d)-1))]
}

func copyWedgeGoroutines() string {
	buf := make([]byte, 4<<20)
	buf = buf[:runtime.Stack(buf, true)]
	var keep []string
	for _, g := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(g, "driver/postgres") {
			keep = append(keep, g)
		}
	}
	s := strings.Join(keep, "\n\n")
	if len(s) > 24<<10 {
		s = s[:24<<10] + "\n...(truncated)"
	}
	return s
}

// TestCopyFromCancelledOwnPoolsMaxOpen1: one Pool of MaxOpen 1 per goroutine, so
// the call after a cancelled one runs on the same connection or on its
// replacement and on no other. Every call has its own marker and a deadline of
// 20-420 microseconds (every third call 1-4 ms, so that some reach the server's
// answer), and is followed by a marker SELECT on the same Pool. Asserts: the
// rows of every call are all there or none (never a partial count); a call that
// returned nil committed all of its rows; the marker SELECT is answered with its
// own marker; no backend is left in COPY. Calls that committed their rows and
// still returned an error (the context ended after the server accepted the COPY)
// are counted and logged, not failed: see Pool.CopyFrom.
func TestCopyFromCancelledOwnPoolsMaxOpen1(t *testing.T) {
	for _, mode := range []string{"direct", "loop"} {
		t.Run(mode, func(t *testing.T) { runCopyOwnPools(t, mode) })
	}
}

func runCopyOwnPools(t *testing.T, mode string) {
	dsn := dsnFromEnv(t)
	db := openDB(t)
	table := uniqueTableName(t, "copy_own_"+mode)
	createTable(t, db, table, "g bigint, pad text")
	const workers = 8
	per := copyWedgeIters()
	pad := strings.Repeat("p", 24000)

	pools := make([]*postgres.Pool, workers)
	for w := range pools {
		opts := []postgres.Option{postgres.WithMaxOpen(1)}
		if mode == "loop" {
			opts = append(opts, postgres.WithEngine(celeris.New(celeris.Config{Addr: "127.0.0.1:0"})))
		}
		p, err := postgres.Open(dsn, opts...)
		if err != nil {
			t.Fatal(err)
		}
		pools[w] = p
	}
	hung := false
	defer func() {
		if hung {
			return
		}
		closed := make(chan struct{})
		go func() {
			defer close(closed)
			for _, p := range pools {
				_ = p.Close()
			}
		}()
		select {
		case <-closed:
		case <-time.After(10 * time.Second):
		}
	}()

	type call struct {
		marker int64
		n      int
		got    int64
		err    error
	}
	results := make([][]call, workers)
	inflight := make([]atomic.Int64, workers)
	var markerFails atomic.Int64
	var failMu sync.Mutex
	var failMsgs []string
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pool := pools[w]
			for i := 0; i < per; i++ {
				k := w*per + i
				marker := int64(k + 1)
				n := 2 + k%9
				rows := make([][]any, n)
				for r := range rows {
					rows[r] = []any{marker, pad}
				}
				d := time.Duration(20+(k*37)%401) * time.Microsecond
				if k%3 == 2 {
					d = time.Duration(1+k%4) * time.Millisecond
				}
				inflight[w].Store(time.Now().UnixNano())
				ctx, cancel := context.WithTimeout(context.Background(), d)
				got, err := pool.CopyFrom(ctx, table, []string{"g", "pad"}, postgres.CopyFromSlice(rows))
				cancel()
				results[w] = append(results[w], call{marker, n, got, err})
				mctx, mcancel := context.WithTimeout(context.Background(), 15*time.Second)
				var echo int64
				merr := pool.QueryRow(mctx, "SELECT $1::bigint", -marker).Scan(&echo)
				mcancel()
				inflight[w].Store(0)
				if merr != nil || echo != -marker {
					markerFails.Add(1)
					failMu.Lock()
					if len(failMsgs) < 10 {
						failMsgs = append(failMsgs, fmt.Sprintf("call %d: marker SELECT got %d, err %v (CopyFrom err %v)", k, echo, merr, err))
					}
					failMu.Unlock()
				}
			}
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
wait:
	for {
		select {
		case <-done:
			break wait
		case <-tick.C:
			now := time.Now().UnixNano()
			for w := range inflight {
				if s := inflight[w].Load(); s != 0 && time.Duration(now-s) > copyHangLimit {
					hung = true
					t.Fatalf("worker %d: a call has been in flight for %v: wedged\n%s", w, time.Duration(now-s).Round(time.Millisecond), copyWedgeGoroutines())
				}
			}
		}
	}

	counts := map[int64]int64{}
	rs, err := db.Query(fmt.Sprintf("SELECT g, count(*) FROM %s GROUP BY g", table))
	if err != nil {
		t.Fatal(err)
	}
	for rs.Next() {
		var g, c int64
		if err := rs.Scan(&g, &c); err != nil {
			t.Fatal(err)
		}
		counts[g] = c
	}
	_ = rs.Close()

	var nilCalls, none, committedErr, partial int
	for _, rr := range results {
		for _, c := range rr {
			got := counts[c.marker]
			switch {
			case got != 0 && got != int64(c.n):
				partial++
				t.Errorf("marker %d: %d rows of %d: a partial COPY (err %v)", c.marker, got, c.n, c.err)
			case c.err == nil && got != int64(c.n):
				t.Errorf("marker %d: CopyFrom returned nil and %d rows (want %d), n=%d", c.marker, got, c.n, c.got)
			case c.err == nil:
				nilCalls++
			case got == int64(c.n):
				committedErr++
			default:
				none++
			}
		}
	}
	t.Logf("COPYOWN %s: %d calls (%d workers, MaxOpen 1 each): returned nil and committed %d; error and no rows %d; error although the COPY committed %d; partial %d; marker SELECT failures %d",
		mode, workers*per, workers, nilCalls, none, committedErr, partial, markerFails.Load())
	for _, m := range failMsgs {
		t.Error(m)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		var stuck int
		q := fmt.Sprintf("SELECT count(*) FROM pg_stat_activity WHERE query LIKE 'COPY %%%s%%' AND state <> 'idle'", table)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := db.QueryRowContext(ctx, q).Scan(&stuck)
		cancel()
		if err != nil {
			t.Errorf("pg_stat_activity: %v", err)
			return
		}
		if stuck == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("%d backends are still in COPY on %s 10 s after the run", stuck, table)
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}
