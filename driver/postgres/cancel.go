package postgres

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
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

// cancelDrainTimeout bounds how long driveDirect waits, after a confirmed
// cancel, for the cancelled statement's Error + ReadyForQuery. A var so the
// tests can shorten it.
var cancelDrainTimeout = 30 * time.Second

// errCancelUnconfirmed fails a conn whose cancel exchange did not finish. It
// reaches no caller: the statement's caller gets its own ctx error.
var errCancelUnconfirmed = errors.New("celeris-postgres: cancel request not confirmed by the server")

// errCopyCancelled fails the conn of a COPY FROM whose context ended while the
// call waited for the server's CopyInResponse (see copyFrom; when the reply and
// the context are both ready the select may take the context, so the reply may
// already have arrived). It reaches no caller: the call returns its context's
// error.
var errCopyCancelled = errors.New("celeris-postgres: COPY FROM cancelled while waiting for the server's CopyInResponse; connection closed")

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
