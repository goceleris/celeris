//go:build !race

// testing.Benchmark + race detector scales b.N too aggressively on slow
// CI runners (see sibling file in driver/redis). Allocation budgets are
// only meaningful on the non-race hot path.

package postgres

import (
	"context"
	"net"
	"os"
	"runtime"
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

	// What a query costs depends on the path: Linux has the sync fast path
	// (syncLoop: the caller polls its own reply), where simpleQuery needs no
	// colsCh; every other platform has none and takes the async path, which
	// allocates the colsCh before it enqueues (simpleQuery), one more. Measured,
	// -count=50 on linux/amd64 at --cpus 2: 14 in 50 of 50 runs, and 14 in 30 of
	// 30 with six CPU hogs beside it; the same bench with syncLoop forced to nil
	// on linux: 15 in 20 of 20; natively on darwin/arm64: 15 in 3 of 3; the Windows
	// portable-fallback job logged 15. It is the path, not noise (celeris#1104).
	query1col1rowBudget := int64(14)
	if runtime.GOOS != "linux" {
		query1col1rowBudget = 15
	}

	type guard struct {
		name   string
		budget int64
		bench  func(b *testing.B)
	}

	guards := []guard{
		// 14 on linux (15 elsewhere, see above), what the driver costs: celeris#890 item 91 measured 14 allocs/op on a
		// linux/arm64 container (20 of 20 runs) and on the GitHub amd64 runner (run
		// 39074958353) against the aspirational 4 this row held until then, which
		// never failed because no CI step set TESTING_STRICT_ALLOC_BUDGETS=1. The
		// allocation-guard step of ci.yml sets it now, so 14 is the bound.
		{name: "Query_1col_1row", budget: query1col1rowBudget, bench: benchQuery1col1row},
		{name: "Pool_AcquireRelease", budget: 0, bench: benchPoolAcquireRelease},
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
			got := res.AllocsPerOp()
			t.Logf("allocs/op=%d (budget %d) — %s", got, g.budget, res)
			if strict && got > g.budget {
				t.Fatalf("allocs/op %d exceeds budget %d (strict mode)", got, g.budget)
			}
		})
	}
}

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
				_ = writeMsg(c, protocol.BackendDataRow, buildDataRow([][]byte{[]byte("7")}))
				_ = writeCommandComplete(c, "SELECT 1")
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
