package postgres

import (
	"context"
	"database/sql/driver"
	"encoding/binary"
	"net"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/internal/eventloop"
	"github.com/goceleris/celeris/driver/postgres/protocol"
)

// extendedFakeServer scripts the minimum server behaviour needed to drive a
// Prepare + Exec round-trip against the fake PG harness. It handles the
// sequences:
//
//   - Parse + Describe('S', name) + Sync →
//     ParseComplete + ParameterDescription(OIDs) + NoData + ReadyForQuery
//   - Bind + Execute + Sync →
//     BindComplete + CommandComplete(tag) + ReadyForQuery
//
// Enough to satisfy the driver's Prepare / Exec state machines while
// allocating nothing in the hot paths under test. Simple Query 'Q' is also
// handled so Ping/ResetSession piggy-back. Terminate returns.
func extendedFakeServer(t testing.TB, paramOIDs []uint32, execTag string) func(net.Conn) {
	return func(c net.Conn) {
		fakePGTrustStartupB(t, c, 1, 2, func(c net.Conn) {
			handleExtendedStream(c, paramOIDs, execTag)
		})
	}
}

func handleExtendedStream(c net.Conn, paramOIDs []uint32, execTag string) {
	for {
		typ, _, err := readMsg(c)
		if err != nil {
			return
		}
		switch typ {
		case protocol.MsgTerminate:
			return
		case protocol.MsgParse:
			// Emit ParseComplete immediately. Pipelined messages like
			// Parse+Bind+Execute+Sync then produce the correct
			// ordering (ParseComplete → BindComplete → ... → RFQ on Sync).
			_ = writeMsg(c, protocol.BackendParseComplete, nil)
		case protocol.MsgDescribe:
			// Describe 'S' → ParameterDescription + NoData. ParseComplete
			// was emitted by the Parse branch already.
			_ = writeMsg(c, protocol.BackendParameterDesc, buildParameterDescription(paramOIDs))
			_ = writeMsg(c, protocol.BackendNoData, nil)
		case protocol.MsgBind:
			_ = writeMsg(c, protocol.BackendBindComplete, nil)
		case protocol.MsgExecute:
			_ = writeCommandComplete(c, execTag)
		case protocol.MsgSync:
			_ = writeReadyForQuery(c, 'I')
		case protocol.MsgQuery:
			_ = writeCommandComplete(c, "SELECT 0")
			_ = writeReadyForQuery(c, 'I')
		case protocol.MsgClose:
			_ = writeMsg(c, protocol.BackendCloseComplete, nil)
		}
	}
}

func buildParameterDescription(oids []uint32) []byte {
	out := make([]byte, 2+4*len(oids))
	binary.BigEndian.PutUint16(out[:2], uint16(len(oids)))
	for i, o := range oids {
		binary.BigEndian.PutUint32(out[2+i*4:], o)
	}
	return out
}

// TestExtendedFakeHarness verifies the fake server handles Parse + Describe
// + Sync correctly by calling PrepareContext + ExecContext.
func TestExtendedFakeHarness(t *testing.T) {
	addr := startFakePG(t, extendedFakeServer(t, []uint32{protocol.OIDInt8, protocol.OIDText}, "INSERT 0 1"))
	prov, err := eventloop.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer eventloop.Release(prov)
	host, port, _ := net.SplitHostPort(addr)
	dsn := DSN{Host: host, Port: port, User: "u", Options: Options{SSLMode: "disable", StatementCacheSize: 16}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := dialConn(ctx, prov, nil, dsn, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()

	stmt, err := c.PrepareContext(ctx, "INSERT INTO t(id, name) VALUES($1, $2)")
	if err != nil {
		t.Fatalf("PrepareContext: %v", err)
	}
	if n := stmt.NumInput(); n != 2 {
		t.Errorf("NumInput=%d, want 2", n)
	}

	args := []driver.NamedValue{
		{Ordinal: 1, Value: int64(1)},
		{Ordinal: 2, Value: "x"},
	}
	res, err := stmt.(*pgStmt).ExecContext(ctx, args)
	if err != nil {
		t.Fatalf("ExecContext: %v", err)
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		t.Errorf("RowsAffected=%d, want 1", n)
	}
}

// benchPreparedExec measures allocs/op for a prepared-statement Exec against
// the extended-protocol fake. We Prepare once outside the loop so the
// measurement only includes the Bind + Execute + Sync round trip.
func benchPreparedExec(b *testing.B) {
	addr := startFakePGBench(b, extendedFakeServer(b, []uint32{protocol.OIDInt8, protocol.OIDText}, "INSERT 0 1"))
	prov, err := eventloop.Resolve(nil)
	if err != nil {
		b.Fatal(err)
	}
	defer eventloop.Release(prov)
	host, port, _ := net.SplitHostPort(addr)
	dsn := DSN{Host: host, Port: port, User: "u", Options: Options{SSLMode: "disable", StatementCacheSize: 16}}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := dialConn(ctx, prov, nil, dsn, 0)
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = c.Close() }()

	stmt, err := c.PrepareContext(ctx, "INSERT INTO t(id, name) VALUES($1, $2)")
	if err != nil {
		b.Fatal(err)
	}
	ps := stmt.(*pgStmt)

	args := []driver.NamedValue{
		{Ordinal: 1, Value: int64(1)},
		{Ordinal: 2, Value: "x"},
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := ps.ExecContext(ctx, args); err != nil {
			b.Fatal(err)
		}
	}
}

// TestAllocBudgetsPreparedExec reports allocations-per-op for a prepared
// INSERT-style Exec against the extended-protocol fake harness. The test
// behaves like TestAllocBudgets: default mode logs + does not fail;
// TESTING_STRICT_ALLOC_BUDGETS=1 enforces the budget.
func TestAllocBudgetsPreparedExec(t *testing.T) {
	strict := os.Getenv("TESTING_STRICT_ALLOC_BUDGETS") == "1"
	const budget int64 = 32 // aspirational — tightened later by the opt loop.

	res := testing.Benchmark(benchPreparedExec)
	got := res.AllocsPerOp()
	t.Logf("PreparedExec allocs/op=%d (budget %d) — %s", got, budget, res)
	if strict && got > budget {
		t.Fatalf("allocs/op %d exceeds budget %d (strict mode)", got, budget)
	}
}

// writeErrorResponse builds and writes a PG ErrorResponse ('E') with the
// given SQLSTATE code and message.
func writeErrorResponse(c net.Conn, code, msg string) error {
	var payload []byte
	payload = append(payload, 'S')
	payload = append(payload, "ERROR\x00"...)
	payload = append(payload, 'C')
	payload = append(payload, code...)
	payload = append(payload, 0)
	payload = append(payload, 'M')
	payload = append(payload, msg...)
	payload = append(payload, 0)
	payload = append(payload, 0) // terminator
	return writeMsg(c, protocol.BackendErrorResponse, payload)
}

// extendedFakeServerWithStaleStmt scripts a fake PG that:
//   - handles the initial Prepare (Parse+Describe+Sync) normally
//   - on the first Bind for the named stmt, returns ErrorResponse 26000
//   - handles the re-prepare (Parse+Describe+Sync) normally
//   - handles the retry Bind+Execute+Sync normally
//
// This exercises the re-prepare-on-miss path in extendedExec/extendedQuery.
func extendedFakeServerWithStaleStmt(t *testing.T, paramOIDs []uint32, execTag string) func(net.Conn) {
	var firstBind atomic.Bool
	return func(c net.Conn) {
		fakePGTrustStartup(t, c, 1, 2, func(c net.Conn) {
			// Buffer responses so they are flushed atomically on Sync,
			// avoiding edge-triggered epoll split-read issues.
			var buf []byte
			enqueueMsg := func(typ byte, payload []byte) {
				msg := make([]byte, 5+len(payload))
				msg[0] = typ
				binary.BigEndian.PutUint32(msg[1:5], uint32(4+len(payload)))
				copy(msg[5:], payload)
				buf = append(buf, msg...)
			}
			flush := func() {
				if len(buf) > 0 {
					_, _ = c.Write(buf)
					buf = buf[:0]
				}
			}
			errored := false
			for {
				typ, _, err := readMsg(c)
				if err != nil {
					return
				}
				switch typ {
				case protocol.MsgTerminate:
					return
				case protocol.MsgParse:
					enqueueMsg(protocol.BackendParseComplete, nil)
				case protocol.MsgDescribe:
					enqueueMsg(protocol.BackendParameterDesc, buildParameterDescription(paramOIDs))
					enqueueMsg(protocol.BackendNoData, nil)
				case protocol.MsgBind:
					if !firstBind.Swap(true) {
						errored = true
						var payload []byte
						payload = append(payload, 'S')
						payload = append(payload, "ERROR\x00"...)
						payload = append(payload, 'C')
						payload = append(payload, "26000\x00"...)
						payload = append(payload, 'M')
						payload = append(payload, "prepared statement celst_1 does not exist\x00"...)
						payload = append(payload, 0)
						enqueueMsg(protocol.BackendErrorResponse, payload)
					} else {
						enqueueMsg(protocol.BackendBindComplete, nil)
					}
				case protocol.MsgExecute:
					if !errored {
						enqueueMsg(protocol.BackendCommandComplete, append([]byte(execTag), 0))
					}
				case protocol.MsgSync:
					enqueueMsg(protocol.BackendReadyForQuery, []byte{'I'})
					flush()
					errored = false
				case protocol.MsgQuery:
					enqueueMsg(protocol.BackendCommandComplete, append([]byte("SELECT 0"), 0))
					enqueueMsg(protocol.BackendReadyForQuery, []byte{'I'})
					flush()
				case protocol.MsgClose:
					enqueueMsg(protocol.BackendCloseComplete, nil)
				}
			}
		})
	}
}

// TestRepreparOnMiss verifies that extendedExec transparently re-prepares a
// server-side statement when the server returns SQLSTATE 26000 (e.g. after
// DISCARD ALL dropped it).
func TestReprepareOnMiss(t *testing.T) {
	addr := startFakePG(t, extendedFakeServerWithStaleStmt(t, []uint32{protocol.OIDInt8, protocol.OIDText}, "INSERT 0 1"))
	prov, err := eventloop.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer eventloop.Release(prov)
	host, port, _ := net.SplitHostPort(addr)
	dsn := DSN{Host: host, Port: port, User: "u", Options: Options{SSLMode: "disable", StatementCacheSize: 16}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := dialConn(ctx, prov, nil, dsn, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()

	// Prepare the statement — this succeeds and caches "celst_1".
	stmt, err := c.PrepareContext(ctx, "INSERT INTO t(id, name) VALUES($1, $2)")
	if err != nil {
		t.Fatalf("PrepareContext: %v", err)
	}

	// First Exec: server returns 26000 on Bind. The driver should
	// transparently re-prepare and retry.
	args := []driver.NamedValue{
		{Ordinal: 1, Value: int64(1)},
		{Ordinal: 2, Value: "x"},
	}
	res, err := stmt.(*pgStmt).ExecContext(ctx, args)
	if err != nil {
		t.Fatalf("ExecContext (re-prepare path): %v", err)
	}
	if res == nil {
		t.Fatal("ExecContext returned nil result with nil error")
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		t.Errorf("RowsAffected=%d, want 1", n)
	}

	// Second Exec: should succeed immediately (no 26000).
	res, err = stmt.(*pgStmt).ExecContext(ctx, args)
	if err != nil {
		t.Fatalf("ExecContext (cached path): %v", err)
	}
	n, _ = res.RowsAffected()
	if n != 1 {
		t.Errorf("RowsAffected=%d, want 1", n)
	}
}
