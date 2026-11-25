package postgres

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/driver/postgres/protocol"
)

// A Pool conn in direct mode waits in a socket read that does not look at its
// context. A context WITH a deadline is enforced through the socket's read
// deadline; a context without one (cancel-only) is not watched at all, as in
// v1.5.11. These tests use a backend that sends NOTHING for a statement until a
// CancelRequest reaches it (no partial reply that would wake the read), and ask
// that a context whose deadline passes mid-statement ends the call with the
// context's error; that the conn is reused after a confirmed cancel exchange (and
// not before the exchange ended); that it is discarded when the exchange is not
// confirmed; and that a cancel-only context does not interrupt the statement.

type silentPG struct {
	addr string
	hold time.Duration // how long a cancel socket stays open; < 0: until the test ends
	// slowDelay > 0: the slow statement answers (CommandComplete) after this long
	// on its own, without waiting for a cancel.
	slowDelay time.Duration

	mu       sync.Mutex
	log      []string
	sessions int
	cancelCh chan struct{}
	done     chan struct{}
}

func startSilentPG(t *testing.T, hold time.Duration) *silentPG {
	t.Helper()
	f := &silentPG{hold: hold, cancelCh: make(chan struct{}, 16), done: make(chan struct{})}
	f.addr = startFakePG(t, f.serve)
	t.Cleanup(func() { close(f.done) })
	return f
}

func (f *silentPG) record(ev string) {
	f.mu.Lock()
	f.log = append(f.log, ev)
	f.mu.Unlock()
}

func (f *silentPG) index(ev string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, e := range f.log {
		if e == ev {
			return i
		}
	}
	return -1
}

func (f *silentPG) events() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.log...)
}

func (f *silentPG) sessionCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sessions
}

func (f *silentPG) serve(c net.Conn) {
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
		select {
		case f.cancelCh <- struct{}{}:
		default:
		}
		if f.hold < 0 {
			<-f.done
		} else {
			select {
			case <-time.After(f.hold):
			case <-f.done:
			}
		}
		f.record("cancel-closed")
		return
	}
	f.mu.Lock()
	f.sessions++
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
		if q == "SELECT slow" && f.slowDelay > 0 {
			select {
			case <-time.After(f.slowDelay):
			case <-f.done:
				return
			}
			_ = writeCommandComplete(c, "SELECT 1")
			_ = writeReadyForQuery(c, 'I')
			continue
		}
		if q == "SELECT slow" {
			select {
			case <-f.cancelCh:
			case <-f.done:
				return
			}
			_ = writeMsg(c, protocol.BackendErrorResponse, []byte("SERROR\x00VERROR\x00C57014\x00Mcanceling statement due to user request\x00\x00"))
			_ = writeReadyForQuery(c, 'I')
			continue
		}
		_ = writeCommandComplete(c, "SELECT 1")
		_ = writeReadyForQuery(c, 'I')
	}
}

func openDirectPool(t *testing.T, addr string) *Pool {
	t.Helper()
	host, port, _ := net.SplitHostPort(addr)
	p, err := Open("postgres://u@"+host+":"+port+"/d?sslmode=disable", WithMaxOpen(1))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// ctxEnds returns a context that ends after d by deadline or by cancel().
func ctxEnds(kind string, d time.Duration) (context.Context, context.CancelFunc) {
	if kind == "deadline" {
		return context.WithTimeout(context.Background(), d)
	}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(d, cancel)
	return ctx, cancel
}

func runSlow(ctx context.Context, p *Pool, query bool) error {
	if !query {
		_, err := p.ExecContext(ctx, "SELECT slow")
		return err
	}
	rows, err := p.QueryContext(ctx, "SELECT slow")
	if err != nil {
		return err
	}
	for rows.Next() {
	}
	err = rows.Err()
	_ = rows.Close()
	return err
}

func TestDirectPoolEnforcesAContextDeadlineMidStatement(t *testing.T) {
	for _, kind := range []string{"deadline"} {
		for _, stmt := range []string{"exec", "query"} {
			t.Run(kind+"/"+stmt, func(t *testing.T) {
				f := startSilentPG(t, 100*time.Millisecond)
				p := openDirectPool(t, f.addr)
				if _, err := p.ExecContext(context.Background(), "SELECT warm"); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := ctxEnds(kind, 100*time.Millisecond)
				defer cancel()
				errc := make(chan error, 1)
				go func() { errc <- runSlow(ctx, p, stmt == "query") }()
				var err error
				select {
				case err = <-errc:
				case <-time.After(3 * time.Second):
					t.Fatal("the context ended mid-statement and the call did not return")
				}
				if want := ctx.Err(); !errors.Is(err, want) {
					t.Fatalf("err = %v, want the context's error (%v)", err, want)
				}
				if _, err := p.ExecContext(context.Background(), "SELECT next"); err != nil {
					t.Fatalf("the next statement failed: %v", err)
				}
				if got := f.sessionCount(); got != 1 {
					t.Fatalf("sessions = %d, want 1: a conn whose cancel exchange was confirmed is reused (%v)", got, f.events())
				}
				closed, next := f.index("cancel-closed"), f.index("query:SELECT next")
				if closed < 0 || next < 0 || closed > next {
					t.Fatalf("the next statement reached the backend before the cancel socket closed: %v", f.events())
				}
			})
		}
	}
}

func TestDirectPoolDiscardsTheConnWhenTheCancelIsNotConfirmed(t *testing.T) {
	old := cancelExchangeTimeout
	cancelExchangeTimeout = 150 * time.Millisecond
	t.Cleanup(func() { cancelExchangeTimeout = old })
	for _, kind := range []string{"deadline"} {
		t.Run(kind, func(t *testing.T) {
			f := startSilentPG(t, -1)
			p := openDirectPool(t, f.addr)
			if _, err := p.ExecContext(context.Background(), "SELECT warm"); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := ctxEnds(kind, 50*time.Millisecond)
			defer cancel()
			errc := make(chan error, 1)
			go func() { errc <- runSlow(ctx, p, false) }()
			var err error
			select {
			case err = <-errc:
			case <-time.After(3 * time.Second):
				t.Fatal("the call did not return after the cancel deadline")
			}
			if want := ctx.Err(); !errors.Is(err, want) || errors.Is(err, ErrBadConn) {
				t.Fatalf("err = %v, want the context's error (%v) and not ErrBadConn", err, want)
			}
			if _, err := p.ExecContext(context.Background(), "SELECT next"); err != nil {
				t.Fatalf("the next statement failed: %v", err)
			}
			if got := f.sessionCount(); got != 2 {
				t.Fatalf("sessions = %d, want 2: the conn with a cancel possibly in flight is discarded", got)
			}
		})
	}
}

// A context that can only be cancelled (no deadline: a cancel-only stdlib
// context, the HTTP/2 request context) is not watched while the statement runs:
// the call returns when the server answers, with the statement's own result, and
// no CancelRequest is sent (v1.5.11 behaviour; celeris#1375).
func TestDirectPoolDoesNotInterruptAStatementForACancelOnlyContext(t *testing.T) {
	f := startSilentPG(t, 0)
	f.slowDelay = 300 * time.Millisecond
	p := openDirectPool(t, f.addr)
	if _, err := p.ExecContext(context.Background(), "SELECT warm"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	start := time.Now()
	_, err := p.ExecContext(ctx, "SELECT slow")
	if err != nil {
		t.Fatalf("err = %v: the statement was interrupted by a cancel-only context", err)
	}
	if el := time.Since(start); el < 250*time.Millisecond {
		t.Fatalf("returned after %v, before the server answered (300 ms)", el)
	}
	if f.index("cancel-request") >= 0 {
		t.Fatalf("a CancelRequest was sent for a cancel-only context: %v", f.events())
	}
}

// deadlineCtx is a context from outside package context with a Done channel of
// its own (never closed) and a deadline, like the timeout middleware's lazy
// context: Err reports the deadline from the clock.
type deadlineCtx struct {
	done chan struct{}
	dl   time.Time
}

func (c *deadlineCtx) Deadline() (time.Time, bool) { return c.dl, true }
func (c *deadlineCtx) Done() <-chan struct{}       { return c.done }
func (c *deadlineCtx) Value(any) any               { return nil }
func (c *deadlineCtx) Err() error {
	if !time.Now().Before(c.dl) {
		return context.DeadlineExceeded
	}
	return nil
}

// The deadline of a context of any type is enforced through the socket's read
// deadline, and the timeout is decided by the clock against Deadline(): a context
// whose Err() is still nil at the instant the read times out must not make the
// conn a network failure.
func TestDirectPoolEnforcesTheDeadlineOfAForeignContextAndDecidesByTheClock(t *testing.T) {
	f := startSilentPG(t, 50*time.Millisecond)
	p := openDirectPool(t, f.addr)
	if _, err := p.ExecContext(context.Background(), "SELECT warm"); err != nil {
		t.Fatal(err)
	}
	// Err stays nil, as a timer context's can for a moment after the deadline.
	ctx := &lateErrCtx{deadlineCtx{done: make(chan struct{}), dl: time.Now().Add(100 * time.Millisecond)}}
	errc := make(chan error, 1)
	go func() { _, err := p.ExecContext(ctx, "SELECT slow"); errc <- err }()
	select {
	case err := <-errc:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want context.DeadlineExceeded", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the deadline of a foreign context was not enforced")
	}
	if _, err := p.ExecContext(context.Background(), "SELECT next"); err != nil {
		t.Fatalf("the next statement failed: %v", err)
	}
	if got := f.sessionCount(); got != 1 {
		t.Fatalf("sessions = %d, want 1: the timeout was taken for a network failure and the conn was discarded (%v)", got, f.events())
	}
}

// lateErrCtx never reports an error from Err: only the clock says it is over.
type lateErrCtx struct{ deadlineCtx }

func (c *lateErrCtx) Err() error { return nil }
