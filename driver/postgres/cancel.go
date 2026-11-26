package postgres

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"time"
)

// cancelRequestCode is the magic int32 that identifies a CancelRequest, from
// the PostgreSQL v3 protocol docs (80877102 = (1234 << 16) | 5678).
const cancelRequestCode int32 = 80877102

// buildCancelRequest produces the 16-byte CancelRequest payload:
//
//	int32 length=16, int32 code=cancelRequestCode, int32 pid, int32 secret.
//
// Exposed for testing.
func buildCancelRequest(pid, secret int32) []byte {
	buf := make([]byte, 16)
	binary.BigEndian.PutUint32(buf[0:4], 16)
	binary.BigEndian.PutUint32(buf[4:8], uint32(cancelRequestCode))
	binary.BigEndian.PutUint32(buf[8:12], uint32(pid))
	binary.BigEndian.PutUint32(buf[12:16], uint32(secret))
	return buf
}

// cancelExchangeTimeout bounds the whole cancel exchange: dial, write, and the
// wait for the server to close the cancel socket. A local postmaster answers
// in well under a millisecond; a remote one in about two round trips. Expiry
// costs a reconnect, never correctness: the conn is discarded (see
// pgConn.cancelRunning). A var so the tests can shorten it.
var cancelExchangeTimeout = time.Second

// cancelDrainTimeout bounds how long a call waits, after a confirmed cancel,
// for the cancelled statement's Error + ReadyForQuery: driveDirect, and the
// ctx branch of waitForQueryRows (as wait()'s 30 s does for the other
// statements). A call whose context ended returns once the server answers the
// cancel, and at the latest this long after the cancel exchange (the
// connection is then failed and discarded). A var so the tests can shorten it.
var cancelDrainTimeout = 30 * time.Second

// errCancelUnconfirmed fails a conn whose cancel exchange did not finish. It
// reaches no caller: the statement's caller gets its own ctx error.
var errCancelUnconfirmed = errors.New("celeris-postgres: cancel request not confirmed by the server")

// sendCancelRequest dials a fresh short-lived TCP connection to addr,
// transmits a CancelRequest and waits for the server to close the socket.
// Does not use the event loop.
//
// The wait is what makes a cancel safe to follow with another statement. A
// backend ignores a cancel only while it is idle, so a cancel that reaches it
// after the cancelled statement ended, while it runs the NEXT statement on the
// same conn, fails that innocent statement with SQLSTATE 57014. The server
// process that reads the cancel packet sends SIGINT to the backend and only
// then exits, which closes the socket (REL_14 processCancelRequest, REL_18
// ProcessCancelRequestPacket), so once the socket is closed the signal is
// already pending in the backend, which consumes it before it starts a
// statement the client sends afterwards (PostgreSQL's command loop checks for
// interrupts right after it reads a command and before it clears
// DoingCommandRead, which is how a cancel that arrives while idle is dropped).
// Behind a pooler the close says only what the pooler does with it (PgBouncer
// forwards the cancel over a server connection of its own).
//
// ctx is not used: the exchange outlives the context whose expiry started it.
// It returns nil only when the server closed the socket (EOF or reset) within
// cancelExchangeTimeout; any other outcome (dial or write error, deadline)
// is an error, and the caller must then assume a cancel may still be in
// flight.
func sendCancelRequest(_ context.Context, addr *net.TCPAddr, pid, secret int32) error {
	deadline := time.Now().Add(cancelExchangeTimeout)
	d := net.Dialer{Deadline: deadline}
	conn, err := d.Dial("tcp", addr.String())
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(deadline)
	if _, err = conn.Write(buildCancelRequest(pid, secret)); err != nil {
		return err
	}
	// The server sends nothing back: it processes the request and closes
	// the socket, which reads as EOF. A reset (PgBouncer closes the cancel
	// socket of a client it has no session for that way) is the same
	// answer: the peer closed after it took the request, nothing of ours
	// is left in flight on this socket.
	var buf [64]byte
	for {
		_, err = conn.Read(buf[:])
		if err == io.EOF || isConnReset(err) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// cancelRunning is what every ctx.Done branch runs before it waits for the
// cancelled statement: it sends the CancelRequest and returns once the
// exchange finished. It runs on the caller's goroutine and the statement's
// method does not return before it does, so Pool.release and database/sql's
// putConn only ever see the conn after the exchange, and the next statement
// cannot be written before then.
//
// It reports whether the conn may be reused. When the exchange did not finish
// (deadline, dial or write error) a cancel may still be in flight; the conn is
// failed (closed is set, so Pool.release discards it and database/sql's
// IsValid drops it; it never reports ErrBadConn, which database/sql would
// retry on a statement that may have run) and the caller returns its ctx error.
// A conn without a BackendKeyData sends no cancel and stays usable.
func (c *pgConn) cancelRunning() bool {
	if c.pid == 0 || c.secret == 0 {
		return true
	}
	if err := sendCancelRequest(context.Background(), c.addr, c.pid, c.secret); err != nil {
		c.failAll(errCancelUnconfirmed)
		return false
	}
	return true
}

// testHookCtxWatch, when set, runs at the start of the callback of armCtxWatch,
// before it acts: a test holds a callback in flight across the end of its
// statement with it (it sleeps), or counts the callbacks that ran (each one is a
// statement whose watch could not be stopped, and so was waited for). It is nil in
// production.
var testHookCtxWatch atomic.Pointer[func()]

// testHookCtxWatchArmed, when set, runs at the end of armCtxWatch, once the
// callback is registered with the context: a test that must end the context only
// after the watch is armed (so that the callback is certain to be started, and the
// statement's end certain to wait for it) gates on it. It is nil in production.
var testHookCtxWatchArmed atomic.Pointer[func()]

// ctxWatch is the wake of a direct conn's blocked read on the end of the
// caller's context (armCtxWatch), the design of driver/redis's and
// driver/memcached's armWake. It lives on the conn, and its callback is built
// once per conn, so that a statement allocates none of it: what a statement
// whose context can end pays is context.AfterFunc's own registration and stop.
//
// The handshake is what makes it safe. A callback that sets a read deadline in
// the past races with everything else that sets one on the same socket: the cancel
// drain of cancelDrainDirect (which sets its own, a bound), and the next statement
// on the conn. So the statement never returns, and the drain never starts,
// before settleCtxWatch has run: stop() returned true (the callback never runs
// and never will), or it returned false and the callback has been waited for
// (it has run to its end, and nothing of the context is left to touch the socket).
// Pair armCtxWatch with settleCtxWatch in one goroutine: a direct conn is pinned
// to one caller at a time.
type ctxWatch struct {
	fn   func()
	stop func() bool // non-nil from armCtxWatch to settleCtxWatch
	// ran has capacity 1: the callback signals it when it has run to its end, and
	// settleCtxWatch waits on it whenever stopping the callback failed.
	ran chan struct{}
	// fired is written by the callback before it signals ran and read after the
	// wait for ran, which orders the two: the callback set a read deadline of "now".
	fired bool
	// deadlineSet: driveDirect set the read deadline to the context's deadline,
	// which endCtxWatch clears.
	deadlineSet bool
}

// armCtxWatch arranges that, when ctx ends, the read deadline of the direct conn
// c becomes "now", which wakes the read of driveDirect blocked on a statement
// that is still running. It arms nothing, and costs the Done() call, for a
// context that can never end (ctx.Done() == nil: Background, a value context over
// it) and for one that has already ended (driveDirect's loop sees ctx.Err() before
// it reads). It reports whether it armed one; the caller defers endCtxWatch then.
func (c *pgConn) armCtxWatch(ctx context.Context) bool {
	if ctx.Done() == nil || ctx.Err() != nil {
		return false
	}
	w := &c.cw
	if w.fn == nil {
		tcp := c.tcp
		w.ran = make(chan struct{}, 1)
		w.fn = func() {
			if h := testHookCtxWatch.Load(); h != nil {
				(*h)()
			}
			_ = tcp.SetReadDeadline(time.Now())
			w.fired = true
			select {
			case w.ran <- struct{}{}:
			default:
			}
		}
	}
	w.fired = false
	w.stop = context.AfterFunc(ctx, w.fn)
	if h := testHookCtxWatchArmed.Load(); h != nil {
		(*h)()
	}
	return true
}

// settleCtxWatch ends the wake armed by armCtxWatch; it does nothing if none is
// armed. When the callback could not be stopped, the context has ended and the
// callback has been started on a goroutine of its own: it is waited for, so that
// it cannot set a deadline after this returns, whether that would be in the cancel
// drain that follows or in the next statement on the conn. A deadline it set is
// left in place (cancelDrainDirect replaces it, endCtxWatch clears it).
func (c *pgConn) settleCtxWatch() {
	w := &c.cw
	if w.stop == nil {
		return
	}
	stop := w.stop
	w.stop = nil
	if !stop() {
		<-w.ran
	}
}

// endCtxWatch is driveDirect's deferred end of a statement's context: it settles
// the callback and clears the read deadline the statement or the callback set, so
// that the socket is left without one (a conn that is closed is left alone).
func (c *pgConn) endCtxWatch() {
	c.settleCtxWatch()
	w := &c.cw
	if (w.deadlineSet || w.fired) && !c.closed.Load() {
		_ = c.tcp.SetReadDeadline(time.Time{})
	}
	w.deadlineSet, w.fired = false, false
}
