package postgres

import (
	"context"
	"encoding/binary"
	"net"
	"os"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/internal/eventloop"
	"github.com/goceleris/celeris/driver/postgres/protocol"
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
		budget int64
		bench  func(b *testing.B)
	}

	guards := []guard{
		{name: "Query_1col_1row", budget: 4, bench: benchQuery1col1row},
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

func startFakePGBench(tb testing.TB, handler func(net.Conn)) string {
	tb.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go handler(c)
		}
	}()
	return ln.Addr().String()
}

func fakePGTrustStartupB(tb testing.TB, c net.Conn, pid, secret int32, post func(net.Conn)) {
	tb.Helper()
	defer func() { _ = c.Close() }()
	// Read startup message.
	lenBuf := make([]byte, 4)
	if _, err := readFull(c, lenBuf); err != nil {
		return
	}
	n := int(binary.BigEndian.Uint32(lenBuf))
	if n < 4 {
		return
	}
	body := make([]byte, n-4)
	if _, err := readFull(c, body); err != nil {
		return
	}
	if err := writeAuthOK(c); err != nil {
		return
	}
	if err := writeBackendKeyData(c, pid, secret); err != nil {
		return
	}
	if err := writeReadyForQuery(c, 'I'); err != nil {
		return
	}
	if post != nil {
		post(c)
	}
}

func readFull(c net.Conn, buf []byte) (int, error) {
	n := 0
	for n < len(buf) {
		k, err := c.Read(buf[n:])
		if err != nil {
			return n, err
		}
		n += k
	}
	return n, nil
}
