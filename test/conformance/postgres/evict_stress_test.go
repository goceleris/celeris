//go:build postgres

package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A randomized concurrent test of the statement cache under eviction. Many
// goroutines share a small pool; every goroutine runs a random mix of
//
//   - one-row statements with a statement text out of a set larger than the
//     cache holds (every run past the cache size evicts),
//   - multi-row statements,
//   - statements the server cuts short with an error after returning rows,
//   - statements run under a context that expires mid-flight (some already
//     expired), on a pool of their own (see below),
//   - cacheable INSERTs,
//
// each with an argument that is the goroutine's own marker. The test asserts:
// nothing hangs (no progress for stallAfter fails it, with a goroutine dump);
// no reply is lost or crossed (each query returns its own marker and its own
// statement's constants; the INSERTs are all in the table, once); a statement
// that fails with an error does so with the error the server or the context
// gives, and nothing else; and the pool is healthy at the end.
//
// The expired-context statements run on their own pool, never on the conns of
// the marker queries: the CancelRequest the driver sends for an expired context
// is fire-and-forget and can reach the backend after the statement it was for
// has ended, cancelling the next statement on that conn (a separate bug, in
// v1.5.11 too, tracked on its own). On a conn of its own the stray cancel can
// only hit another expired-context statement, whose outcome allows a cancel.
// The price: on that pool a crossed reply shows only when a statement succeeds.
// Eviction inside a transaction and a stale cache entry are in evict_tx_test.go.
//
// Arms: the native Pool in direct mode, the native Pool on the event loop, and
// database/sql (event loop on Linux), each with a cache of 8 statements, plus
// the default cache size (256) with more statements than it holds.
//
//	CELERIS_PG_STRESS_SECONDS   total run time over all arms (default 60; 6 with -short)
//	CELERIS_PG_STRESS_SEED      seed (default: the clock; logged)
func TestStatementCacheEvictionStress(t *testing.T) {
	dsn := dsnFromEnv(t)
	total := 60 * time.Second
	if testing.Short() {
		total = 6 * time.Second
	}
	if s := os.Getenv("CELERIS_PG_STRESS_SECONDS"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			t.Fatalf("CELERIS_PG_STRESS_SECONDS=%q: want a positive integer", s)
		}
		total = time.Duration(n) * time.Second
	}
	seed := uint64(time.Now().UnixNano())
	if s := os.Getenv("CELERIS_PG_STRESS_SEED"); s != "" {
		n, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			t.Fatalf("CELERIS_PG_STRESS_SEED=%q: want an unsigned integer", s)
		}
		seed = n
	}
	t.Logf("seed %d (rerun with CELERIS_PG_STRESS_SEED=%d)", seed, seed)

	type arm struct {
		mode  evictMode
		cache int // statement_cache_size; 0 = the default
		texts int // distinct statement texts
	}
	var arms []arm
	for _, m := range evictModes {
		arms = append(arms, arm{m, 8, 40})
	}
	arms = append(arms, arm{evictModes[0], 0, 400}, arm{evictModes[2], 0, 400})

	db := openDB(t)
	for i, a := range arms {
		name := fmt.Sprintf("%s/cache=%d/texts=%d", a.mode.name, a.cache, a.texts)
		t.Run(name, func(t *testing.T) {
			d := dsn
			if a.cache > 0 {
				d = withParams(dsn, "statement_cache_size="+strconv.Itoa(a.cache))
			}
			table := uniqueTableName(t, "evict_stress")
			createTable(t, db, table, "w int, m bigint, k int")
			s := &stress{
				t: t, db: a.mode.open(t, d, 3), cdb: a.mode.open(t, d, 2), texts: a.texts, table: table,
				seed: seed + uint64(i), workers: 8, runFor: total / time.Duration(len(arms)),
			}
			defer s.db.Close()
			defer s.cdb.Close()
			s.run()
			s.verify(db)
		})
	}
}

// stallAfter is how long the whole arm may go without any goroutine finishing an
// operation before the test calls it a hang. The longest legitimate operation
// is a statement of a few milliseconds.
const stallAfter = 20 * time.Second

type stress struct {
	t       *testing.T
	db      stressDB // marker queries, multi-row, error-after-rows, INSERTs
	cdb     stressDB // expired-context statements only
	texts   int
	table   string
	seed    uint64
	workers int
	runFor  time.Duration

	progress atomic.Int64 // unix nanoseconds of the last finished operation
	failed   atomic.Bool
	mu       sync.Mutex
	failure  string
	inserted []int64 // per worker: INSERTs that returned success
	ops      [6]atomic.Int64
	withRows atomic.Int64 // error-after-rows statements that handed over rows before the error
	cancels  atomic.Int64 // statements that ended with the context error
	pgErrs   atomic.Int64 // statements that ended with the expected server error
}

func (s *stress) fail(format string, args ...any) {
	s.mu.Lock()
	if s.failure == "" {
		s.failure = fmt.Sprintf(format, args...)
	}
	s.mu.Unlock()
	s.failed.Store(true)
}

func (s *stress) run() {
	t := s.t
	s.inserted = make([]int64, s.workers)
	s.progress.Store(time.Now().UnixNano())
	stop := time.Now().Add(s.runFor)
	var wg sync.WaitGroup
	for w := 0; w < s.workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(s.seed, uint64(w)))
			for n := int64(0); time.Now().Before(stop) && !s.failed.Load(); n++ {
				s.once(rng, w, n)
				s.progress.Store(time.Now().UnixNano())
			}
		}(w)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-done:
			if s.failure != "" {
				t.Fatalf("%s", s.failure)
			}
			if s.withRows.Load() == 0 {
				t.Fatalf("no error-after-rows statement handed over its rows before the error: the check for their columns never ran")
			}
			t.Logf("ops: marker=%d multirow=%d error-after-rows=%d (%d with rows) cancel=%d insert=%d; context errors %d, server errors %d",
				s.ops[0].Load(), s.ops[1].Load(), s.ops[2].Load(), s.withRows.Load(), s.ops[3].Load(), s.ops[4].Load(), s.cancels.Load(), s.pgErrs.Load())
			return
		case <-tick.C:
			if time.Since(time.Unix(0, s.progress.Load())) > stallAfter {
				dumpGoroutines(t)
				t.Fatalf("hung: no operation finished for %v (the last failure, if any: %q)", stallAfter, s.failure)
			}
		}
	}
}

// once runs one random operation of worker w. marker is unique to the call.
func (s *stress) once(rng *rand.Rand, w int, n int64) {
	marker := int64(w)<<40 | n
	k := rng.IntN(s.texts)
	switch p := rng.IntN(100); {
	case p < 44:
		s.ops[0].Add(1)
		s.markerQuery(w, marker, k)
	case p < 60:
		s.ops[1].Add(1)
		s.multiRow(w, marker, k, 2+rng.IntN(15))
	case p < 80:
		s.ops[2].Add(1)
		s.errorAfterRows(w, marker, k, 2+rng.IntN(4))
	case p < 86:
		s.ops[3].Add(1)
		s.cancelled(w, marker, k, time.Duration(rng.IntN(4))*time.Millisecond)
	default:
		s.ops[4].Add(1)
		s.insert(w, marker, k)
	}
}

func (s *stress) ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 2*stallAfter)
}

// markerQuery: one row, its own marker, text and constant back.
func (s *stress) markerQuery(w int, marker int64, k int) {
	ctx, cancel := s.ctx()
	defer cancel()
	text := "w" + strconv.Itoa(w) + "-" + strconv.FormatInt(marker, 10)
	q := fmt.Sprintf("SELECT $1::int8 AS m%d, %d::int4 AS k, $2::text AS s", k, k)
	var m int64
	var kk int32
	var str string
	if err := queryOne(ctx, s.db, q, []any{marker, text}, &m, &kk, &str); err != nil {
		s.fail("marker query %q marker %d: %v", q, marker, err)
		return
	}
	if m != marker || int(kk) != k || str != text {
		s.fail("marker query %q: got (%d, %d, %q), want (%d, %d, %q): a reply was lost or crossed", q, m, kk, str, marker, k, text)
	}
}

// multiRow: nrows rows, each with the marker.
func (s *stress) multiRow(w int, marker int64, k, nrows int) {
	ctx, cancel := s.ctx()
	defer cancel()
	q := fmt.Sprintf("SELECT g::int4, $1::int8 AS m%d, g::text FROM generate_series(1,%d) AS g", k, nrows)
	rows, err := s.db.Query(ctx, q, marker)
	if err != nil {
		s.fail("multi-row %q marker %d: %v", q, marker, err)
		return
	}
	defer func() { _ = rows.Close() }()
	got := 0
	for rows.Next() {
		var g int32
		var m int64
		var gs string
		if err := rows.Scan(&g, &m, &gs); err != nil {
			s.fail("multi-row %q row %d: %v", q, got+1, err)
			return
		}
		got++
		if int(g) != got || m != marker || gs != strconv.Itoa(got) {
			s.fail("multi-row %q row %d: got (%d, %d, %q), want (%d, %d, %q): a reply was lost or crossed", q, got, g, m, gs, got, marker, strconv.Itoa(got))
			return
		}
	}
	if err := rows.Err(); err != nil {
		s.fail("multi-row %q: %v", q, err)
		return
	}
	if got != nrows {
		s.fail("multi-row %q: %d rows, want %d", q, got, nrows)
	}
}

// errorAfterRows: the server raises division_by_zero at row failAt. The rows
// before it, when the driver hands them over, are right (column description
// included: an int4 reads as its number into *string); the error is the
// server's, 22012, at Query or at the end of the rows.
func (s *stress) errorAfterRows(w int, marker int64, k, failAt int) {
	ctx, cancel := s.ctx()
	defer cancel()
	q := fmt.Sprintf("SELECT n::int4 AS a%d, $1::int8 AS m, n::text FROM generate_series(1,8) AS n WHERE 1/(n-%d) > -100", k, failAt)
	rows, err := s.db.Query(ctx, q, marker)
	if err != nil {
		if e := pgCode(err, "22012"); e != nil {
			s.fail("error-after-rows %q marker %d at Query: %v", q, marker, e)
			return
		}
		s.pgErrs.Add(1)
		return
	}
	defer func() { _ = rows.Close() }()
	got := 0
	for rows.Next() {
		var a string // *string of an int4: the case of the lost description
		var m int64
		var nt string
		if err := rows.Scan(&a, &m, &nt); err != nil {
			s.fail("error-after-rows %q row %d: %v", q, got+1, err)
			return
		}
		got++
		if a != strconv.Itoa(got) || m != marker || nt != strconv.Itoa(got) {
			s.fail("error-after-rows %q row %d: got (%q, %d, %q), want (%d, %d, %d)", q, got, a, m, nt, got, marker, got)
			return
		}
		if got >= failAt {
			s.fail("error-after-rows %q: row %d arrived, the error is at row %d", q, got, failAt)
			return
		}
	}
	if got != failAt-1 {
		s.fail("error-after-rows %q marker %d: %d rows before the error at row %d, want %d", q, marker, got, failAt, failAt-1)
		return
	}
	if e := pgCode(rows.Err(), "22012"); e != nil {
		s.fail("error-after-rows %q marker %d, %d rows: %v", q, marker, got, e)
		return
	}
	s.withRows.Add(1)
	s.pgErrs.Add(1)
}

// cancelled: a statement that sleeps 3 ms under a context that expires after
// d (0: already expired). It finishes with its own marker or with the context's
// error (a network timeout when it expired during a dial, or the server's
// cancel, 57014); nothing else.
func (s *stress) cancelled(w int, marker int64, k int, d time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	q := fmt.Sprintf("SELECT pg_sleep($1::float8) IS NOT NULL AS s%d, $2::int8", k)
	var ok bool
	var m int64
	err := queryOne(ctx, s.cdb, q, []any{0.003, marker}, &ok, &m)
	switch {
	case err == nil:
		if m != marker {
			s.fail("cancelled %q: got marker %d, want %d: a reply was lost or crossed", q, m, marker)
		}
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled), isNetTimeout(err):
		// isNetTimeout: the pool had to dial a new conn and the context expired
		// during the dial ("dial tcp ...: i/o timeout").
		s.cancels.Add(1)
	case pgCode(err, "57014") == nil:
		s.cancels.Add(1)
	default:
		s.fail("cancelled %q after %v: unexpected error %v", q, d, err)
	}
}

// isNetTimeout reports a network timeout error (a dial or an I/O deadline).
func isNetTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// insert: a cacheable write with its own marker; counted only on success.
func (s *stress) insert(w int, marker int64, k int) {
	ctx, cancel := s.ctx()
	defer cancel()
	q := fmt.Sprintf("INSERT INTO %s(w, m, k) VALUES ($1, $2, %d)", s.table, k)
	if err := s.db.Exec(ctx, q, w, marker); err != nil {
		s.fail("insert %q marker %d: %v", q, marker, err)
		return
	}
	s.inserted[w]++
}

// verify: every INSERT that returned is in the table exactly once, and the pool
// still answers, on every conn it holds, with each query's own marker.
func (s *stress) verify(admin *sql.DB) {
	t := s.t
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var want int64
	for _, n := range s.inserted {
		want += n
	}
	var cnt, distinct int64
	if err := admin.QueryRowContext(ctx, fmt.Sprintf("SELECT count(*), count(DISTINCT m) FROM %s", s.table)).Scan(&cnt, &distinct); err != nil {
		t.Fatalf("count: %v", err)
	}
	if cnt != want || distinct != want {
		t.Fatalf("the table holds %d rows (%d distinct markers), %d INSERTs returned success", cnt, distinct, want)
	}
	for w, n := range s.inserted {
		var c int64
		if err := admin.QueryRowContext(ctx, fmt.Sprintf("SELECT count(*) FROM %s WHERE w = $1", s.table), w).Scan(&c); err != nil {
			t.Fatalf("count worker %d: %v", w, err)
		}
		if c != n {
			t.Fatalf("worker %d: %d rows in the table, %d INSERTs returned success", w, c, n)
		}
	}
	// Healthy: concurrent queries across every conn, each with its own marker.
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				marker := int64(1)<<50 | int64(g)<<20 | int64(i)
				var m int64
				if err := queryOne(ctx, s.db, fmt.Sprintf("SELECT $1::int8 AS final%d", i%5), []any{marker}, &m); err != nil {
					errs <- fmt.Errorf("final query: %w", err)
					return
				}
				if m != marker {
					errs <- fmt.Errorf("final query: got marker %d, want %d", m, marker)
					return
				}
			}
		}(g)
	}
	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(stallAfter):
		dumpGoroutines(t)
		t.Fatalf("hung: the final queries did not finish in %v", stallAfter)
	}
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}
