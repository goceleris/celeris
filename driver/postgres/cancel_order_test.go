package postgres

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/internal/looptest"
	"github.com/goceleris/celeris/internal/driver/postgres/protocol"
)

// The stray cancel: the CancelRequest of an expired context is delivered to
// the backend on its own socket, and a backend ignores a cancel only when it is
// idle at that instant. A conn that carried its next statement before the
// postmaster had acted on the cancel could therefore see that statement fail
// with 57014 ("canceling statement due to user request"). The tests below
// replace the server with a fake postmaster that keeps a cancel socket open for
// a chosen time (a postmaster, or a pooler, that is slow to act) and records,
// in one ordered log, when each cancel socket closed and when each statement
// reached the backend.

const (
	fakeSlowQuery = "SELECT slow"
	fakeNextQuery = "SELECT next"
)

type fakePostmaster struct {
	t    *testing.T
	addr string
	// hold is how long a cancel socket stays open after its request was
	// read; negative keeps it open until the test ends.
	hold time.Duration
	// slowReply is what the fake answers to fakeSlowQuery once a
	// CancelRequest arrived: "error" (57014 + ReadyForQuery), "complete"
	// (the statement had already ended: CommandComplete + ReadyForQuery) or
	// "never" (the statement keeps running; no reply at all).
	slowReply string

	mu       sync.Mutex
	log      []string
	startups int
	cancelCh chan struct{}
	// slowCh gets one token each time fakeSlowQuery reaches the backend: the
	// tests end their context from it, so no clock decides whether the cancel
	// exists.
	slowCh chan struct{}
	done   chan struct{}
}

func startFakePostmaster(t *testing.T, hold time.Duration, slowReply string) *fakePostmaster {
	t.Helper()
	f := &fakePostmaster{t: t, hold: hold, slowReply: slowReply, cancelCh: make(chan struct{}, 16), slowCh: make(chan struct{}, 16), done: make(chan struct{})}
	f.addr = startFakePG(t, f.serve)
	t.Cleanup(func() { close(f.done) })
	return f
}

func (f *fakePostmaster) record(ev string) {
	f.mu.Lock()
	f.log = append(f.log, ev)
	f.mu.Unlock()
}

// index returns the position of the first event with the given prefix, or -1.
func (f *fakePostmaster) index(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, ev := range f.log {
		if strings.HasPrefix(ev, prefix) {
			return i
		}
	}
	return -1
}

func (f *fakePostmaster) events() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.log...)
}

func (f *fakePostmaster) sessions() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.startups
}

func (f *fakePostmaster) serve(c net.Conn) {
	defer func() { _ = c.Close() }()
	var lenBuf [4]byte
	if _, err := io.ReadFull(c, lenBuf[:]); err != nil {
		return
	}
	n := int(binary.BigEndian.Uint32(lenBuf[:]))
	if n < 8 {
		return
	}
	body := make([]byte, n-4)
	if _, err := io.ReadFull(c, body); err != nil {
		return
	}
	if n == 16 && int32(binary.BigEndian.Uint32(body[0:4])) == cancelRequestCode {
		f.record("cancel-request")
		f.cancelCh <- struct{}{}
		if f.hold < 0 {
			<-f.done
		} else {
			select {
			case <-time.After(f.hold):
			case <-f.done:
			}
		}
		// Logged before the Close that the deferred call performs: a
		// client that saw the socket close cannot have sent anything that
		// the log shows earlier than this entry.
		f.record("cancel-closed")
		return
	}
	f.mu.Lock()
	f.startups++
	f.mu.Unlock()
	if writeAuthOK(c) != nil || writeBackendKeyData(c, 7, 9) != nil || writeReadyForQuery(c, 'I') != nil {
		return
	}
	for {
		typ, payload, err := readMsg(c)
		if err != nil || typ == protocol.MsgTerminate {
			return
		}
		if typ != protocol.MsgQuery {
			continue
		}
		q := string(bytes.TrimRight(payload, "\x00"))
		f.record("query:" + q)
		if q == fakeSlowQuery {
			select {
			case f.slowCh <- struct{}{}:
			default:
			}
		}
		if q == fakeSlowQuery {
			// NoticeResponses while the statement "runs": its response
			// arrives in pieces; they keep coming, so the context ends
			// before or after the first one, whichever path the conn takes to
			// the cancel (the wake of its read, or the context seen when a
			// piece arrives).
			tick := time.NewTicker(50 * time.Millisecond)
			cancelled := false
			for !cancelled {
				select {
				case <-tick.C:
					_ = writeMsg(c, protocol.BackendNoticeResponse, []byte("SNOTICE\x00VNOTICE\x00C00000\x00Mstill running\x00\x00"))
				case <-f.cancelCh:
					if f.slowReply == "never" {
						// The cancel was never acted on: the statement keeps running.
						<-f.done
						tick.Stop()
						return
					}
					cancelled = true
				case <-f.done:
					tick.Stop()
					return
				}
			}
			tick.Stop()
			if f.slowReply == "error" {
				_ = writeMsg(c, protocol.BackendErrorResponse, []byte("SERROR\x00VERROR\x00C57014\x00Mcanceling statement due to user request\x00\x00"))
			} else {
				_ = writeCommandComplete(c, "SELECT 0")
			}
			_ = writeReadyForQuery(c, 'I')
			continue
		}
		_ = writeCommandComplete(c, "SELECT 1")
		_ = writeReadyForQuery(c, 'I')
	}
}

// cancelArm is one way to reach the driver with a single conn.
type cancelArm struct {
	name string
	open func(t *testing.T, addr string) func(ctx context.Context, q string, query bool) error
}

func cancelArms() []cancelArm {
	dsnFor := func(addr string) string {
		host, port, _ := net.SplitHostPort(addr)
		return "postgres://u@" + host + ":" + port + "/d?sslmode=disable"
	}
	poolRun := func(p *Pool) func(ctx context.Context, q string, query bool) error {
		return func(ctx context.Context, q string, query bool) error {
			if !query {
				_, err := p.ExecContext(ctx, q)
				return err
			}
			rows, err := p.QueryContext(ctx, q)
			if err != nil {
				return err
			}
			for rows.Next() {
			}
			err = rows.Err()
			_ = rows.Close()
			return err
		}
	}
	return []cancelArm{
		{"pool-direct", func(t *testing.T, addr string) func(context.Context, string, bool) error {
			p, err := Open(dsnFor(addr), WithMaxOpen(1))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = p.Close() })
			return poolRun(p)
		}},
		{"pool-loop", func(t *testing.T, addr string) func(context.Context, string, bool) error {
			p, err := Open(dsnFor(addr), WithMaxOpen(1), WithEngine(looptest.Unstarted()))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = p.Close() })
			return poolRun(p)
		}},
		{"database-sql", func(t *testing.T, addr string) func(context.Context, string, bool) error {
			db, err := sql.Open(DriverName, dsnFor(addr))
			if err != nil {
				t.Fatal(err)
			}
			db.SetMaxOpenConns(1)
			t.Cleanup(func() { _ = db.Close() })
			return func(ctx context.Context, q string, query bool) error {
				if !query {
					_, err := db.ExecContext(ctx, q)
					return err
				}
				rows, err := db.QueryContext(ctx, q)
				if err != nil {
					return err
				}
				for rows.Next() {
				}
				err = rows.Err()
				_ = rows.Close()
				return err
			}
		}},
	}
}

// warmUp opens the conn before the short context starts: Open and sql.Open are
// lazy, and a dial that took longer than the 30 ms context on a busy runner
// would fail in acquire, before any cancel exists.
func warmUp(t *testing.T, run func(context.Context, string, bool) error, query bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := run(ctx, "SELECT warm", query); err != nil {
		t.Fatalf("warm-up statement: %v", err)
	}
}

// Ordering: the next statement of a cancelled conn must not reach the backend
// before the cancel socket has closed. The fake postmaster is slow to close it;
// the log must show the close first, on every route that reaches a conn.
func TestCancelFinishesBeforeNextStatement(t *testing.T) {
	for _, arm := range cancelArms() {
		for _, kind := range []string{"exec", "query"} {
			for _, reply := range []string{"error", "complete"} {
				t.Run(arm.name+"/"+kind+"/"+reply, func(t *testing.T) {
					f := startFakePostmaster(t, 250*time.Millisecond, reply)
					run := arm.open(t, f.addr)
					isQuery := kind == "query"
					warmUp(t, run, isQuery)

					// The context ends when the slow statement reached the backend,
					// not after a fixed time: no clock decides whether a cancel exists.
					ctx, cancel := context.WithCancel(context.Background())
					go func() {
						<-f.slowCh
						cancel()
					}()
					_ = run(ctx, fakeSlowQuery, isQuery) // an error or not: the statement raced its cancel
					cancel()

					nctx, ncancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer ncancel()
					if err := run(nctx, fakeNextQuery, isQuery); err != nil {
						t.Fatalf("the next statement failed: %v", err)
					}
					closed, next := f.index("cancel-closed"), f.index("query:"+fakeNextQuery)
					if closed < 0 || next < 0 || closed > next {
						t.Fatalf("the next statement reached the backend before the cancel socket closed: %v", f.events())
					}
				})
			}
		}
	}
}

// Expiry: when the cancel socket is never closed, the exchange gives up after
// its short deadline. That is not an error for the caller (it gets its own
// ctx error, not a driver one, and not ErrBadConn, which database/sql would
// retry), and the conn that may have a cancel in flight is not reused: the
// next statement runs on a new session.
func TestCancelExpiryDiscardsTheConn(t *testing.T) {
	old := cancelExchangeTimeout
	cancelExchangeTimeout = 150 * time.Millisecond
	t.Cleanup(func() { cancelExchangeTimeout = old })
	for _, arm := range cancelArms() {
		for _, kind := range []string{"exec", "query"} {
			t.Run(arm.name+"/"+kind, func(t *testing.T) {
				f := startFakePostmaster(t, -1, "never")
				run := arm.open(t, f.addr)
				isQuery := kind == "query"
				warmUp(t, run, isQuery)

				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				go func() {
					<-f.slowCh
					cancel()
				}()
				errc := make(chan error, 1)
				go func() { errc <- run(ctx, fakeSlowQuery, isQuery) }()
				var err error
				select {
				case err = <-errc:
				case <-time.After(5 * time.Second):
					t.Fatal("the cancelled call did not return after the cancel deadline")
				}
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("err = %v, want the caller's context.Canceled", err)
				}
				if errors.Is(err, driver.ErrBadConn) || errors.Is(err, ErrBadConn) {
					t.Fatalf("err = %v: ErrBadConn would make database/sql retry a statement that may have run", err)
				}
				nctx, ncancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer ncancel()
				if err := run(nctx, fakeNextQuery, isQuery); err != nil {
					t.Fatalf("the next statement failed: %v", err)
				}
				if got := f.sessions(); got != 2 {
					t.Fatalf("sessions = %d, want 2: the conn with a possible cancel in flight was reused (%v)", got, f.events())
				}
			})
		}
	}
}

// The exchange itself: sendCancelRequest returns only once the server closed
// the cancel socket, and reports a socket that stays open as unconfirmed.
func TestSendCancelRequestWaitsForServerClose(t *testing.T) {
	const hold = 200 * time.Millisecond
	f := startFakePostmaster(t, hold, "error")
	addr, err := net.ResolveTCPAddr("tcp", f.addr)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := sendCancelRequest(context.Background(), addr, 7, 9); err != nil {
		t.Fatalf("sendCancelRequest: %v", err)
	}
	if el := time.Since(start); el < hold-20*time.Millisecond {
		t.Fatalf("returned after %v, before the server closed the socket (%v)", el, hold)
	}
	if f.index("cancel-closed") < 0 {
		t.Fatalf("returned before the server closed the cancel socket: %v", f.events())
	}
}

func TestSendCancelRequestUnconfirmedAtDeadline(t *testing.T) {
	old := cancelExchangeTimeout
	cancelExchangeTimeout = 150 * time.Millisecond
	t.Cleanup(func() { cancelExchangeTimeout = old })
	f := startFakePostmaster(t, -1, "never")
	addr, err := net.ResolveTCPAddr("tcp", f.addr)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err = sendCancelRequest(context.Background(), addr, 7, 9)
	if err == nil {
		t.Fatal("a cancel socket the server never closed was reported as finished")
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("took %v with a %v deadline", el, cancelExchangeTimeout)
	}
}

// A pooler may answer a CancelRequest by resetting the socket instead of
// closing it (PgBouncer does for a client it has no session for): that is as
// final as an EOF, the exchange is over.
func TestSendCancelRequestResetIsConfirmed(t *testing.T) {
	ln, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		c, err := ln.AcceptTCP()
		if err != nil {
			return
		}
		var buf [16]byte
		_, _ = io.ReadFull(c, buf[:])
		_ = c.SetLinger(0) // Close now sends RST
		_ = c.Close()
	}()
	if err := sendCancelRequest(context.Background(), ln.Addr().(*net.TCPAddr), 7, 9); err != nil {
		t.Fatalf("a reset after the request was reported as unconfirmed: %v", err)
	}
}
