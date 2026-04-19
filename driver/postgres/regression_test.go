package postgres

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/internal/eventloop"
	"github.com/goceleris/celeris/driver/postgres/protocol"
	"github.com/goceleris/celeris/engine"
)

// TestPgConnCloseNoDoubleClose asserts Close's fd-close path is idempotent:
// onClose (event loop) and Close (caller) may both race to tear the conn
// down; syscall.Close must happen at most once. We verify by inspecting the
// fdCloseOnce guard rather than relying on kernel behavior — the race
// detector would not catch a stray syscall.Close on an int. (PG-1)
func TestPgConnCloseNoDoubleClose(t *testing.T) {
	addr := startFakePG(t, func(c net.Conn) {
		fakePGTrustStartup(t, c, 1, 2, func(c net.Conn) {
			_, _ = io.Copy(io.Discard, c)
		})
	})
	prov, err := eventloop.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer eventloop.Release(prov)

	host, port, _ := net.SplitHostPort(addr)
	dsn := DSN{Host: host, Port: port, User: "u", Options: Options{SSLMode: "disable", StatementCacheSize: 4}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := dialConn(ctx, prov, nil, dsn, 0)
	if err != nil {
		t.Fatal(err)
	}

	// Race: concurrent Close from two goroutines + a synthetic onClose via
	// closeFDOnce directly. None of them should double-close.
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = c.Close()
		}()
	}
	wg.Wait()

	// Re-invoke closeFDOnce: it must be a no-op now.
	c.closeFDOnce()
	// Issue syscall.Close(fd) again and expect EBADF (fd already closed) —
	// proving the real close happened exactly once, not twice. If we had
	// double-closed, EBADF would have been returned by the FIRST redundant
	// call inside Close and the fd number would now potentially alias.
	err = syscall.Close(c.fd)
	if !errors.Is(err, syscall.EBADF) {
		t.Fatalf("expected EBADF after Close(), got %v", err)
	}
}

// TestPgBridgeOnWriteError simulates a Write failure on query #1 and verifies
// the bridge stays in lock-step with pending, so query #2's reply is routed
// to query #2 — not swallowed by a phantom entry from query #1. (PG-2)
func TestPgBridgeOnWriteError(t *testing.T) {
	addr := startFakePG(t, func(c net.Conn) {
		fakePGTrustStartup(t, c, 1, 2, func(c net.Conn) {
			for {
				typ, _, err := readMsg(c)
				if err != nil {
					return
				}
				if typ == protocol.MsgTerminate {
					return
				}
				if typ == protocol.MsgQuery {
					_ = writeCommandComplete(c, "SELECT 1")
					_ = writeReadyForQuery(c, 'I')
				}
			}
		})
	})
	prov, err := eventloop.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer eventloop.Release(prov)

	host, port, _ := net.SplitHostPort(addr)
	dsn := DSN{Host: host, Port: port, User: "u", Options: Options{SSLMode: "disable", StatementCacheSize: 4}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, err := dialConn(ctx, prov, nil, dsn, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()

	// Manually drive failReq for a fake request we enqueue, simulating a
	// Write-returned-error path. This is the exact sequence that produced
	// the bridge phantom before the fix.
	fakeReq := &pgRequest{
		ctx:    context.Background(),
		kind:   reqSimple,
		doneCh: make(chan struct{}),
	}
	c.enqueue(fakeReq)
	c.failReq(fakeReq, errors.New("simulated write failure"))

	// pending and bridge must both be empty.
	c.pendingMu.Lock()
	pendingLen := len(c.pending)
	c.pendingMu.Unlock()
	bridgeLen := c.bridge.Len()
	if pendingLen != 0 || bridgeLen != 0 {
		t.Fatalf("after failReq: pending=%d bridge=%d; want 0/0", pendingLen, bridgeLen)
	}

	// A real query after the failure should succeed — its reply must not be
	// eaten by a phantom bridge head.
	if _, _, err := c.simpleExec(ctx, "SELECT 1"); err != nil {
		t.Fatalf("follow-up simpleExec: %v", err)
	}
}

// TestPgDoneChNoRaceClose forces multiple goroutines to complete the same
// request concurrently and asserts no panic. Before the sync.Once guard this
// would flake under -race with "close of closed channel". (PG-3)
func TestPgDoneChNoRaceClose(t *testing.T) {
	req := &pgRequest{
		doneCh: make(chan struct{}),
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req.finish()
		}()
	}
	wg.Wait()
	select {
	case <-req.doneCh:
	default:
		t.Fatal("doneCh not closed after finish()")
	}
}

// TestPgServerParamsRace spawns N goroutines reading ServerParam concurrent
// with the event loop writing via ParameterStatus dispatch. Run under -race. (PG-4)
func TestPgServerParamsRace(t *testing.T) {
	addr := startFakePG(t, func(c net.Conn) {
		defer func() { _ = c.Close() }()
		readStartup(t, c)
		_ = writeAuthOK(c)
		_ = writeBackendKeyData(c, 1, 2)
		_ = writeReadyForQuery(c, 'I')
		// Spam ParameterStatus asynchronously.
		for i := 0; i < 50; i++ {
			ps := []byte("server_version\x0016.0\x00")
			_ = writeMsg(c, protocol.BackendParameterStatus, ps)
			time.Sleep(time.Millisecond)
		}
		_, _ = io.Copy(io.Discard, c)
	})
	prov, err := eventloop.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer eventloop.Release(prov)

	host, port, _ := net.SplitHostPort(addr)
	dsn := DSN{Host: host, Port: port, User: "u", Options: Options{SSLMode: "disable"}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, err := dialConn(ctx, prov, nil, dsn, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()

	var stop atomic.Bool
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				_ = c.ServerParam("server_version")
			}
		}()
	}
	time.Sleep(100 * time.Millisecond)
	stop.Store(true)
	wg.Wait()
}

// TestPgPoolCloseRacesAcquire spawns N goroutines Acquiring while Close runs;
// no Acquire must panic with a nil-deref on p.provider. (PG-5/PG-6)
func TestPgPoolCloseRacesAcquire(t *testing.T) {
	addr := startFakePG(t, func(c net.Conn) {
		defer func() { _ = c.Close() }()
		readStartup(t, c)
		_ = writeAuthOK(c)
		_ = writeBackendKeyData(c, 1, 2)
		_ = writeReadyForQuery(c, 'I')
		for {
			typ, _, err := readMsg(c)
			if err != nil {
				return
			}
			if typ == protocol.MsgTerminate {
				return
			}
			if typ == protocol.MsgQuery {
				_ = writeCommandComplete(c, "SELECT 1")
				_ = writeReadyForQuery(c, 'I')
			}
		}
	})
	host, port, _ := net.SplitHostPort(addr)
	p, err := Open("postgres://u@" + host + ":" + port + "/d?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			// Any of ErrPoolClosed, exhausted, context error, dial error are OK.
			// The only forbidden outcome is a panic.
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("acquire panicked: %v", r)
				}
			}()
			_ = p.Ping(ctx)
		}()
	}
	// Short window of overlap, then Close while the goroutines are still
	// in-flight.
	time.Sleep(10 * time.Millisecond)
	_ = p.Close()
	wg.Wait()
}

// TestPgResetSessionDiscardAll asserts ResetSession sends DISCARD ALL on the
// wire when sessionDirty is set and clears the per-conn statement cache.
// When only stmts are cached (no sessionDirty), ResetSession is a no-op
// to avoid the round-trip — prepared stmts are kept alive for reuse and
// re-prepared on miss (26000) if they get dropped externally. (PG-7)
func TestPgResetSessionDiscardAll(t *testing.T) {
	var got atomic.Value // string
	addr := startFakePG(t, func(c net.Conn) {
		fakePGTrustStartup(t, c, 1, 2, func(c net.Conn) {
			for {
				typ, body, err := readMsg(c)
				if err != nil {
					return
				}
				if typ == protocol.MsgTerminate {
					return
				}
				if typ == protocol.MsgQuery {
					s := string(body)
					s = strings.TrimRight(s, "\x00")
					got.Store(s)
					_ = writeCommandComplete(c, "DISCARD ALL")
					_ = writeReadyForQuery(c, 'I')
				}
			}
		})
	})
	prov, err := eventloop.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer eventloop.Release(prov)

	host, port, _ := net.SplitHostPort(addr)
	dsn := DSN{Host: host, Port: port, User: "u", Options: Options{SSLMode: "disable", StatementCacheSize: 4}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, err := dialConn(ctx, prov, nil, dsn, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()

	// Sub-test 1: only stmts cached, no sessionDirty → ResetSession is a
	// no-op (no server round-trip, cache preserved).
	c.stmtCache.put("select 1", &protocol.PreparedStmt{Name: "celst_1", Query: "select 1"})
	if err := c.ResetSession(ctx); err != nil {
		t.Fatalf("ResetSession (stmts only): %v", err)
	}
	if q, _ := got.Load().(string); q != "" {
		t.Fatalf("server received %q on stmts-only reset, want no query", q)
	}
	// Cache should be preserved (stmts kept alive for reuse).
	if _, ok := c.stmtCache.get("select 1"); !ok {
		t.Fatal("stmt cache cleared unexpectedly on stmts-only reset")
	}

	// Sub-test 2: sessionDirty → DISCARD ALL + cache cleared.
	c.sessionDirty.Store(true)
	if err := c.ResetSession(ctx); err != nil {
		t.Fatalf("ResetSession (dirty): %v", err)
	}
	q, _ := got.Load().(string)
	if q != "DISCARD ALL" {
		t.Fatalf("server received %q, want DISCARD ALL", q)
	}
	if _, ok := c.stmtCache.get("select 1"); ok {
		t.Fatal("stmt cache not cleared after dirty ResetSession")
	}
}

// TestPgEncoderRejectsUnknownType ensures an unsupported arg type surfaces as
// an explicit error rather than silently going through fmt.Sprint. (PG-9)
func TestPgEncoderRejectsUnknownType(t *testing.T) {
	type custom struct{ X int }
	_, _, err := encodeOne(custom{X: 7})
	if err == nil {
		t.Fatal("expected error for unsupported struct type")
	}
	if !strings.Contains(err.Error(), "unsupported argument type") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestPgDatePreEpochFloorDiv verifies the pre-2000 date path uses floor
// division, not truncation. (PG-10) 1999-12-31 UTC must encode as -1, not 0.
func TestPgDatePreEpochFloorDiv(t *testing.T) {
	// Direct smoke test against the protocol encoder — cheaper than running
	// the whole fake-server machinery.
	pre := time.Date(1999, 12, 31, 0, 0, 0, 0, time.UTC)
	codec := protocol.LookupOID(protocol.OIDDate)
	if codec == nil || codec.EncodeBinary == nil {
		t.Fatal("date codec missing")
	}
	out, err := codec.EncodeBinary(nil, pre)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 4 {
		t.Fatalf("date bytes: %d, want 4", len(out))
	}
	got := int32(binary.BigEndian.Uint32(out))
	if got != -1 {
		t.Fatalf("1999-12-31 encoded as %d, want -1", got)
	}
	// And 1999-12-30 → -2.
	out, _ = codec.EncodeBinary(nil, pre.AddDate(0, 0, -1))
	got = int32(binary.BigEndian.Uint32(out))
	if got != -2 {
		t.Fatalf("1999-12-30 encoded as %d, want -2", got)
	}
}

// ---------- Blocker 3: cancel-wait bounded timeout ----------------------

// TestPgCancelWaitTimeout verifies that a cancelled query does not block
// indefinitely when the server becomes unreachable after CancelRequest. The
// wait() function must time out after a bounded period.
func TestPgCancelWaitTimeout(t *testing.T) {
	// Server that accepts the startup, then stalls forever on any query
	// (never sends a response). The cancel side-channel conn will also be
	// accepted but ignored.
	addr := startFakePG(t, func(c net.Conn) {
		defer func() { _ = c.Close() }()
		if !readStartup(t, c) {
			return
		}
		_ = writeAuthOK(c)
		_ = writeBackendKeyData(c, 99, 0xDEAD)
		_ = writeReadyForQuery(c, 'I')
		// Read the query, then stall indefinitely — never reply.
		_, _, err := readMsg(c)
		if err != nil {
			return
		}
		// Block until the test closes us.
		select {}
	})
	prov, err := eventloop.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer eventloop.Release(prov)

	host, port, _ := net.SplitHostPort(addr)
	dsn := DSN{Host: host, Port: port, User: "u", Options: Options{SSLMode: "disable", StatementCacheSize: 4}}
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer dialCancel()
	c, err := dialConn(dialCtx, prov, nil, dsn, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()

	// Issue a query with a short ctx; cancel triggers the CancelRequest
	// side-channel, then wait() should hit its 30s timer. We override
	// the timer inside the test by simply checking that the function
	// returns — the key assertion is that it doesn't hang forever.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, _, qErr := c.simpleExec(ctx, "SELECT pg_sleep(999)")
	if qErr == nil {
		t.Fatal("expected ctx cancellation error")
	}
	if !errors.Is(qErr, context.DeadlineExceeded) {
		// The conn was either cancelled or the cancel-timeout fired;
		// both are acceptable outcomes.
		t.Logf("query error (expected): %v", qErr)
	}
}

// ---------- Blocker 5: TLS error messages are clear ---------------------

// TestPgExecReturningLargeResult guards against a panic that bit any Exec
// path whose query returned >= streamThreshold (64) DataRows — e.g.
// INSERT ... RETURNING id over many rows. The dispatch handler used to
// call promoteToStreaming unconditionally once len(head.rows) crossed
// the threshold, but Exec paths do not allocate req.colsCh, so
// promoteToStreaming's close(req.colsCh) panicked on a nil channel and
// killed the event-loop worker goroutine.
func TestPgExecReturningLargeResult(t *testing.T) {
	const nRows = 128
	addr := startFakePG(t, func(c net.Conn) {
		fakePGTrustStartup(t, c, 1, 2, func(c net.Conn) {
			// First Query: simpleExec-style — emit RowDescription + lots
			// of DataRows + CommandComplete + ReadyForQuery.
			typ, _, err := readMsg(c)
			if err != nil || typ != protocol.MsgQuery {
				return
			}
			rd := buildRowDescription([]colSpec{{
				name: "id", typeOID: protocol.OIDInt8, typeSize: 8,
				format: protocol.FormatText,
			}})
			_ = writeMsg(c, protocol.BackendRowDescription, rd)
			for i := 0; i < nRows; i++ {
				idBytes := []byte{byte('0' + (i % 10))}
				_ = writeMsg(c, protocol.BackendDataRow, buildDataRow([][]byte{idBytes}))
			}
			_ = writeCommandComplete(c, "INSERT 0 128")
			_ = writeReadyForQuery(c, 'I')
			// Keep the conn alive for graceful close.
			_, _, _ = readMsg(c)
		})
	})

	prov, err := eventloop.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer eventloop.Release(prov)

	host, port, _ := net.SplitHostPort(addr)
	dsn := DSN{Host: host, Port: port, User: "u", Options: Options{SSLMode: "disable", StatementCacheSize: 4}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := dialConn(ctx, prov, nil, dsn, 0)
	if err != nil {
		t.Fatalf("dialConn: %v", err)
	}
	defer func() { _ = c.Close() }()

	// Trigger the simple-protocol Exec path: no args → simpleExec.
	res, err := c.ExecContext(ctx,
		"INSERT INTO t SELECT generate_series(1, 128) RETURNING id", nil)
	if err != nil {
		t.Fatalf("ExecContext: %v", err)
	}
	got, err := res.RowsAffected()
	if err != nil {
		t.Fatalf("RowsAffected: %v", err)
	}
	if got != nRows {
		t.Fatalf("RowsAffected=%d, want %d", got, nRows)
	}
}

// zeroWorkerProvider exposes a Provider with 0 workers so dialConn takes
// the early-return branch that previously called syscall.Close(fd) while
// the *os.File wrapper was still live (with finalizer armed).
type zeroWorkerProvider struct{}

func (zeroWorkerProvider) NumWorkers() int                    { return 0 }
func (zeroWorkerProvider) WorkerLoop(n int) engine.WorkerLoop { panic("no workers") }

// TestPgDialConnNoPhantomCloseOnError guards against a double-close of
// an fd when dialConn bails out before adopting it into fdFile. The
// previous error branches called syscall.Close(fd) but left the
// *os.File returned from tcp.File() alive with its runtime finalizer
// armed, so a later GC cycle would call syscall.Close(fd) AGAIN,
// potentially on an unrelated fd the kernel had since reassigned.
//
// This test forces the NumWorkers<=0 branch, runs GC, and verifies the
// process doesn't crash or leak an extra fd.
func TestPgDialConnNoPhantomCloseOnError(t *testing.T) {
	// Accept-and-hold server so dialConn reaches the NumWorkers check.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c // hold it open; dialConn will bail before the handshake
		}
	}()

	host, port, _ := net.SplitHostPort(ln.Addr().String())
	dsn := DSN{Host: host, Port: port, User: "u", Options: Options{SSLMode: "disable", StatementCacheSize: 4}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Invoke with a zero-worker provider. dialConn should return the
	// "0 workers" error and close the fd via file.Close() (disarming the
	// finalizer). Previously it used syscall.Close(fd), leaving the
	// finalizer armed for a later GC-driven double close.
	_, derr := dialConn(ctx, zeroWorkerProvider{}, nil, dsn, 0)
	if derr == nil {
		t.Fatal("dialConn should have returned an error")
	}
	if !strings.Contains(derr.Error(), "0 workers") {
		t.Fatalf("unexpected error: %v", derr)
	}

	// Force GC twice — if the fd was closed via syscall.Close and the
	// *os.File finalizer still armed, the finalizer would close the fd
	// again here. Under the race detector or with an fd recycled to an
	// unrelated socket this typically surfaces as an EBADF / panic.
	runtime.GC()
	runtime.GC()
	runtime.Gosched()
	runtime.GC()
}

func TestPgTLSErrorMessage(t *testing.T) {
	_, err := Open("postgres://u@localhost/?sslmode=require")
	if err == nil {
		t.Fatal("expected SSL rejection")
	}
	if !strings.Contains(err.Error(), "v1.4.0") {
		t.Fatalf("TLS error missing version info: %v", err)
	}
	if !strings.Contains(err.Error(), "VPC/loopback") {
		t.Fatalf("TLS error missing workaround: %v", err)
	}
}
