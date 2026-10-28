//go:build !race

// testing.Benchmark + race detector scales b.N too aggressively on slow
// CI runners (see sibling file in driver/redis). Allocation budgets are
// only meaningful on the non-race hot path.

package postgres

import (
	"context"
	"net"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/internal/eventloop"
	"github.com/goceleris/celeris/internal/driver/postgres/protocol"
)

// TestAllocBudgets reports current allocations-per-op for the small set
// of Postgres operations that the v1.4.0 profile-driven optimization loop
// is targeting (plan §6.6). Each sub-test runs the relevant benchmark once
// via testing.Benchmark and compares AllocsPerOp against an aspirational
// budget.
//
// Behaviour:
//   - Default: always logs `allocs/op=X (budget N)` so every test run
//     surfaces current allocation profiles, but never fails. Allocation
//     regressions need to be fixed by the optimization pass, not block
//     merges.
//   - With TESTING_STRICT_ALLOC_BUDGETS=1: the test fails if observed
//     allocations exceed the budget. This is for the post-loop CI gate.
//
// Benchmarks use the in-process fakePG harness (see conn_test.go) so these
// tests are hermetic — no live Postgres required.
func TestAllocBudgets(t *testing.T) {
	strict := os.Getenv("TESTING_STRICT_ALLOC_BUDGETS") == "1"

	type guard struct {
		name   string
		budget func() int64 // read after the benchmark ran
		bench  func(b *testing.B)
	}
	fixed := func(n int64) func() int64 { return func() int64 { return n } }

	guards := []guard{
		// What the driver costs: 14 with the sync fast path (the caller polls its own
		// reply), measured by celeris#890 item 91 on linux/arm64 (20 of 20 runs) and on
		// the GitHub amd64 runner against the aspirational 4 this row held until then;
		// the ci.yml allocation-guard step sets TESTING_STRICT_ALLOC_BUDGETS=1 now.
		// Without it simpleQuery allocates the colsCh before it enqueues: 15 (linux
		// with syncLoop nil 20 of 20; darwin 3 of 3; the Windows job logged 15; linux
		// with it: 14 in 50 of 50, and in 30 of 30 under CPU load; celeris#1104). The
		// budget follows the path the bench's conn took (query1col1rowSyncPath), not
		// the OS.
		{name: "Query_1col_1row", budget: func() int64 {
			if query1col1rowSyncPath.Load() {
				return 14
			}
			return 15
		}, bench: benchQuery1col1row},
		{name: "Pool_AcquireRelease", budget: fixed(0), bench: benchPoolAcquireRelease},
		// Prepared exec is intentionally omitted: it requires the extended
		// protocol (Parse/Bind/Execute/Sync) handshake which the existing
		// fake fixtures don't script, and adding that scaffolding here
		// would duplicate a lot of driver logic. The budget (≤2
		// allocs/op) will be covered by the drivercmp suite once live
		// Postgres is wired into CI.
	}

	for _, g := range guards {
		t.Run(g.name, func(t *testing.T) {
			res := testing.Benchmark(g.bench)
			got, budget := res.AllocsPerOp(), g.budget()
			t.Logf("allocs/op=%d (budget %d) — %s", got, budget, res)
			if strict && got > budget {
				t.Fatalf("allocs/op %d exceeds budget %d (strict mode)", got, budget)
			}
		})
	}
}

// fakePGWriteGap is how long the fake backend of benchQuery1col1row waits
// between the writes of one reply (zero: none). A test sets it to model a reply
// that reaches the client in pieces.
var fakePGWriteGap atomic.Int64

func fakePGPause() {
	if g := fakePGWriteGap.Load(); g > 0 {
		time.Sleep(time.Duration(g))
	}
}

// query1col1rowSyncPath is whether the conn benchQuery1col1row ran on took the
// sync fast path (it has a syncLoop), which decides the allocs per query.
var query1col1rowSyncPath atomic.Bool

// benchQuery1col1row measures per-op allocs for QueryContext against a
// fake backend that replies with RowDescription(1 col) + DataRow(1 field)
// + CommandComplete + ReadyForQuery for every Query message.
func benchQuery1col1row(b *testing.B) {
	addr := startFakePGBench(b, func(c net.Conn) {
		fakePGTrustStartupB(b, c, 1, 2, func(c net.Conn) {
			for {
				typ, _, err := readMsg(c)
				if err != nil {
					return
				}
				if typ != protocol.MsgQuery {
					if typ == protocol.MsgTerminate {
						return
					}
					continue
				}
				rd := buildRowDescription([]colSpec{{name: "n", typeOID: protocol.OIDInt4, typeSize: 4, format: protocol.FormatText}})
				_ = writeMsg(c, protocol.BackendRowDescription, rd)
				fakePGPause()
				_ = writeMsg(c, protocol.BackendDataRow, buildDataRow([][]byte{[]byte("7")}))
				fakePGPause()
				_ = writeCommandComplete(c, "SELECT 1")
				fakePGPause()
				_ = writeReadyForQuery(c, 'I')
			}
		})
	})

	prov, err := eventloop.Resolve(nil)
	if err != nil {
		b.Fatal(err)
	}
	defer eventloop.Release(prov)

	host, port, _ := net.SplitHostPort(addr)
	dsn := DSN{
		Host: host, Port: port, User: "u",
		Options: Options{SSLMode: "disable", StatementCacheSize: 16},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := dialConn(ctx, prov, nil, dsn, 0)
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	query1col1rowSyncPath.Store(conn.syncLoop != nil)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rows, err := conn.QueryContext(ctx, "SELECT 7", nil)
		if err != nil {
			b.Fatal(err)
		}
		_ = rows.Close()
	}
}

// benchPoolAcquireRelease measures the steady-state acquire/release cost
// on a pre-warmed idle conn. The hot loop only cycles pool state — the
// underlying conn never actually serves a query, so this isolates the
// idle-list machinery that the optimization loop wants to audit.
func benchPoolAcquireRelease(b *testing.B) {
	addr := startFakePGBench(b, func(c net.Conn) {
		fakePGTrustStartupB(b, c, 1, 2, func(c net.Conn) {
			// Drain whatever the driver sends; we never service it —
			// we only need startup to succeed so acquire() sees an
			// idle conn.
			buf := make([]byte, 4096)
			for {
				if _, err := c.Read(buf); err != nil {
					return
				}
			}
		})
	})

	host, port, _ := net.SplitHostPort(addr)
	dsnStr := "postgres://u@" + host + ":" + port + "/?sslmode=disable"
	p, err := Open(dsnStr, WithMaxOpen(1))
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = p.Close() }()

	// Warm the pool with a single acquire/release so the first hot-loop
	// iteration doesn't pay for the dial.
	ctx := context.Background()
	c, err := p.acquire(ctx)
	if err != nil {
		b.Fatal(err)
	}
	p.release(c)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c, err := p.acquire(ctx)
		if err != nil {
			b.Fatal(err)
		}
		p.release(c)
	}
}

// --- benchmark-grade fake server helpers (mirrors conn_test.go but takes
// testing.TB so *testing.B can drive them).
