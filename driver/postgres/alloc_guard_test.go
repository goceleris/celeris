//go:build !race

// testing.Benchmark + race detector scales b.N too aggressively on slow
// CI runners (see sibling file in driver/redis). Allocation budgets are
// only meaningful on the non-race hot path.

package postgres

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
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
		// What the driver costs for one query whose reply is there when the sync poll
		// reads it: 4, with the sync fast path (the caller polls its own reply), and 5
		// without it (simpleQuery allocates the colsCh before it enqueues; a platform
		// with no sync poll, and a poll that reads only part of the reply, which falls
		// through to the async wait and allocates it there: "Partial or EAGAIN").
		// Measured on linux/arm64 and linux/amd64, with the fake backend answering in
		// one segment from a prebuilt reply (benchQuery1col1rowReply). This row held 14
		// from celeris#890 item 91 to celeris#1104, and 14 was not the driver's cost:
		// two thirds of it was the fake backend's own allocations (the bench counts the
		// whole process), and the backend wrote the reply as four segments, so a poll
		// that read it in pieces, in every query of a run, made it 15 against 14 on a CI
		// runner (celeris#890 allocation guards, twice). The budget follows the path the
		// bench's conn took (query1col1rowSyncPath), not the OS.
		{name: "Query_1col_1row", budget: query1col1rowBudget, bench: benchQuery1col1row},
		// The same query with the reply arriving in pieces (the backend writes its four
		// messages 2 ms apart, so the sync poll reads the first one and falls through to
		// the async wait, where simpleQuery allocates colsCh): the whole reply's 4 and
		// that one. Without the sync poll the colsCh is allocated up front: 5 as well.
		// celeris#1104: the guard above counted 15 against 14 on a CI runner when its
		// backend's four writes, then back to back, were read in pieces; the backend
		// now answers it in one segment, and this row is the pieces' price.
		{name: "Query_1col_1row_reply_in_pieces", budget: fixed(5), bench: benchQuery1col1rowInPieces},
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

// fakePGWriteGap is how long benchQuery1col1rowReply waits between the writes of
// a reply it sends in pieces (apart). A test sets it.
var fakePGWriteGap atomic.Int64

// query1col1rowBudget is the Query_1col_1row budget of the path the bench's
// conn took (see the guard).
func query1col1rowBudget() int64 {
	if query1col1rowSyncPath.Load() {
		return 4
	}
	return 5
}

// query1col1rowSyncPath is whether the conn benchQuery1col1row ran on took the
// sync fast path (it has a syncLoop), which decides the allocs per query.
var query1col1rowSyncPath atomic.Bool

// benchQuery1col1row measures per-op allocs for QueryContext against a
// fake backend that replies with RowDescription(1 col) + DataRow(1 field)
// + CommandComplete + ReadyForQuery for every Query message.
func benchQuery1col1row(b *testing.B) { benchQuery1col1rowReply(b, false) }

// benchQuery1col1rowInPieces is benchQuery1col1row with a backend that writes
// the four messages of its reply 2 ms apart.
func benchQuery1col1rowInPieces(b *testing.B) {
	fakePGWriteGap.Store(int64(2 * time.Millisecond))
	defer fakePGWriteGap.Store(0)
	benchQuery1col1rowReply(b, true)
}

// benchQuery1col1rowReply is benchQuery1col1row with the shape of the reply on
// the wire: one segment (apart false: what postgres sends, it flushes at
// ReadyForQuery) or one segment per message with fakePGWriteGap between them
// (apart true: a reply that reaches the client in pieces).
//
// The reply is built once and the backend reads into one buffer, so that the
// allocations the bench counts (testing.Benchmark counts the whole process) are
// the driver's and not the fake backend's, which used to be about two thirds of
// them and varied with how its four writes were scheduled (celeris#1104).
func benchQuery1col1rowReply(b *testing.B, apart bool) {
	rd := buildRowDescription([]colSpec{{name: "n", typeOID: protocol.OIDInt4, typeSize: 4, format: protocol.FormatText}})
	msgs := [][]byte{
		pgMsg(protocol.BackendRowDescription, rd),
		pgMsg(protocol.BackendDataRow, buildDataRow([][]byte{[]byte("7")})),
		pgMsg(protocol.BackendCommandComplete, append([]byte("SELECT 1"), 0)),
		pgMsg(protocol.BackendReadyForQuery, []byte{'I'}),
	}
	reply := bytes.Join(msgs, nil)
	addr := startFakePGBench(b, func(c net.Conn) {
		fakePGTrustStartupB(b, c, 1, 2, func(c net.Conn) {
			var hdr [5]byte
			body := make([]byte, 256)
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
				if hdr[0] != protocol.MsgQuery {
					if hdr[0] == protocol.MsgTerminate {
						return
					}
					continue
				}
				if !apart {
					_, _ = c.Write(reply)
					continue
				}
				for i, m := range msgs {
					if i > 0 {
						if g := fakePGWriteGap.Load(); g > 0 {
							time.Sleep(time.Duration(g))
						}
					}
					_, _ = c.Write(m)
				}
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
