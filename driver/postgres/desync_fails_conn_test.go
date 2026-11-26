package postgres

// A conn whose reply stream no longer matches its request queue is dead: when a
// state machine meets a message it does not expect for the request at the head
// (a COPY reply to a statement sent through Query or Exec, a stray ReadyForQuery,
// a DataRow out of phase), the conn is failed and replaced, instead of the
// request being popped and the rest of the reply dispatched to whichever request
// is queued next. Each test scripts the server's reply to one request and checks
// the three things the rule promises: the caller gets an error, the conn is
// closed, and a later request never sees a message of the earlier reply.

import (
	"context"
	"database/sql/driver"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/internal/eventloop"
	"github.com/goceleris/celeris/driver/internal/looptest"
	"github.com/goceleris/celeris/internal/driver/postgres/protocol"
)

// dsFrame encodes one backend message.
func dsFrame(typ byte, payload []byte) []byte {
	b := make([]byte, 5+len(payload))
	b[0] = typ
	binary.BigEndian.PutUint32(b[1:5], uint32(4+len(payload)))
	copy(b[5:], payload)
	return b
}

func dsFrames(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

var (
	// dsCopyOut is a CopyOutResponse: text, one column.
	dsCopyOut = dsFrame(protocol.BackendCopyOutResponse, []byte{0, 0, 1, 0, 0})
	// dsCopyIn is a CopyInResponse: text, one column.
	dsCopyIn   = dsFrame(protocol.BackendCopyInResponse, []byte{0, 0, 1, 0, 0})
	dsCopyData = dsFrame(protocol.BackendCopyData, []byte("row\n"))
	dsCopyDone = dsFrame(protocol.BackendCopyDone, nil)
	dsCmdCopy  = dsFrame(protocol.BackendCommandComplete, []byte("COPY 1\x00"))
	dsRFQ      = dsFrame(protocol.BackendReadyForQuery, []byte{'I'})
	dsParseOK  = dsFrame(protocol.BackendParseComplete, nil)
	dsBindOK   = dsFrame(protocol.BackendBindComplete, nil)
	dsCloseOK  = dsFrame(protocol.BackendCloseComplete, nil)
	dsNoData   = dsFrame(protocol.BackendNoData, nil)
	dsParamD0  = dsFrame(protocol.BackendParameterDesc, []byte{0, 0})
	dsDataRow0 = dsFrame(protocol.BackendDataRow, []byte{0, 0})
	dsRowDesc0 = dsFrame(protocol.BackendRowDescription, []byte{0, 0})
	dsTail     = dsFrames(dsCopyData, dsCopyData, dsCopyDone, dsCmdCopy, dsRFQ)
)

// dsDial dials a conn to a fake server whose handler is script: the server does
// the trust startup, reads the first message of the first request, runs reply
// with the client's connection, and then discards whatever else arrives.
func dsDial(t *testing.T, reply func(c net.Conn)) *pgConn {
	t.Helper()
	addr := startFakePG(t, func(c net.Conn) {
		fakePGTrustStartup(t, c, 1, 2, func(c net.Conn) {
			if _, _, err := readMsg(c); err != nil {
				return
			}
			reply(c)
			_, _ = io.Copy(io.Discard, c)
		})
	})
	prov, err := eventloop.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eventloop.Release(prov) })
	host, port, _ := net.SplitHostPort(addr)
	dsn := DSN{Host: host, Port: port, User: "u", Options: Options{SSLMode: "disable", StatementCacheSize: 8}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := dialConn(ctx, prov, nil, dsn, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// dsWrite writes parts to c, with a pause after the first so the client reads
// it alone: the split a large CopyData causes on a real server.
func dsWrite(c net.Conn, split bool, parts ...[]byte) {
	if !split {
		_, _ = c.Write(dsFrames(parts...))
		return
	}
	_, _ = c.Write(parts[0])
	time.Sleep(40 * time.Millisecond)
	_, _ = c.Write(dsFrames(parts[1:]...))
}

type dsCase struct {
	name  string
	parts [][]byte
	// want is a substring every error of the head must contain ("" = any error).
	want string
}

// dsAssertFailed checks the conn is failed and a later request is refused with
// ErrClosed, not handed anything.
func dsAssertFailed(t *testing.T, c *pgConn) {
	t.Helper()
	if !c.closed.Load() {
		t.Fatal("the conn is not closed after its reply stream stopped matching its queue")
	}
	if c.IsValid() {
		t.Fatal("IsValid is true for a failed conn")
	}
	_, _, err := c.simpleExec(dsCtx(t), "SELECT 1")
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("a request on the failed conn: err = %v, want ErrClosed", err)
	}
}

func dsCheckErr(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatal("the request that met an unexpected message returned no error")
	}
	if want != "" && !strings.Contains(err.Error(), want) {
		t.Fatalf("err = %q, want it to contain %q", err, want)
	}
}

const dsCopyWant = "CopyTo"

// dsCtx bounds a request, so that a conn that is wrongly left waiting fails the
// test instead of hanging it.
func dsCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// A COPY through the simple protocol: the CopyOut / CopyIn reply is not a result
// set. The caller is told to use CopyFrom and CopyTo, the conn is closed, and the
// tail of the reply (which may arrive split off the head) is never dispatched.
func TestSimpleQueryUnexpectedMessageFailsConn(t *testing.T) {
	cases := []dsCase{
		{"copy-out", [][]byte{dsCopyOut, dsTail}, dsCopyWant},
		{"copy-in", [][]byte{dsCopyIn}, dsCopyWant},
		{"copy-both", [][]byte{dsFrame(protocol.BackendCopyBothResponse, []byte{0, 0, 1, 0, 0})}, dsCopyWant},
		{"stray-parse-complete", [][]byte{dsParseOK, dsRFQ}, "unexpected message"},
		{"stray-ready-for-query", [][]byte{dsRFQ}, "ReadyForQuery"},
		{"data-row-before-row-description", [][]byte{dsDataRow0, dsRFQ}, "DataRow"},
		{"stray-copy-data", [][]byte{dsCopyData, dsRFQ}, "unexpected message"},
	}
	for _, tc := range cases {
		for _, split := range []bool{false, true} {
			name := tc.name
			if split {
				name += "/split"
			}
			t.Run(name, func(t *testing.T) {
				c := dsDial(t, func(c net.Conn) { dsWrite(c, split, tc.parts...) })
				_, _, err := c.simpleExec(dsCtx(t), "COPY t TO STDOUT")
				dsCheckErr(t, err, tc.want)
				if tc.want == dsCopyWant && !strings.Contains(err.Error(), "CopyFrom") {
					t.Fatalf("err = %q does not name CopyFrom", err)
				}
				if tc.want == dsCopyWant && !errors.Is(err, ErrClosed) {
					t.Fatalf("err = %q does not match ErrClosed", err)
				}
				dsAssertFailed(t, c)
			})
		}
	}
}

// The extended protocol (Parse/Bind/Execute): out-of-phase messages and a
// ReadyForQuery before the reply is complete fail the conn too.
func TestExtendedQueryUnexpectedMessageFailsConn(t *testing.T) {
	cases := []dsCase{
		{"copy-out", [][]byte{dsParseOK, dsBindOK, dsCopyOut, dsTail}, dsCopyWant},
		{"copy-in", [][]byte{dsParseOK, dsBindOK, dsCopyIn}, dsCopyWant},
		{"data-row-before-bind-complete", [][]byte{dsParseOK, dsDataRow0, dsRFQ}, "DataRow"},
		{"ready-for-query-before-result", [][]byte{dsParseOK, dsBindOK, dsRFQ}, "ReadyForQuery"},
		{"ready-for-query-first", [][]byte{dsRFQ}, "ReadyForQuery"},
		{"close-complete", [][]byte{dsParseOK, dsBindOK, dsCloseOK, dsRFQ}, "unexpected message"},
		{"second-parse-complete", [][]byte{dsParseOK, dsParseOK, dsRFQ}, "ParseComplete"},
	}
	for _, tc := range cases {
		for _, split := range []bool{false, true} {
			name := tc.name
			if split {
				name += "/split"
			}
			t.Run(name+"/exec", func(t *testing.T) {
				c := dsDial(t, func(c net.Conn) { dsWrite(c, split, tc.parts...) })
				_, err := c.extendedExec(dsCtx(t), "", "COPY t TO STDOUT", nil)
				dsCheckErr(t, err, tc.want)
				dsAssertFailed(t, c)
			})
			t.Run(name+"/query", func(t *testing.T) {
				c := dsDial(t, func(c net.Conn) { dsWrite(c, split, tc.parts...) })
				rows, err := c.extendedQuery(dsCtx(t), "", "COPY t TO STDOUT", nil)
				if err == nil {
					// The error may come from the rows: read them out.
					dest := make([]driver.Value, len(rows.Columns()))
					for err = nil; err == nil; err = rows.Next(dest) { //revive:disable-line:empty-block
					}
					_ = rows.Close()
					if errors.Is(err, io.EOF) {
						err = nil
					}
				}
				dsCheckErr(t, err, tc.want)
				dsAssertFailed(t, c)
			})
		}
	}
}

// A prepare that meets a stray message returns an error and fails the conn. A
// ReadyForQuery used to be taken as success whatever came before it, which
// returned a statement with no parameter or column metadata.
func TestPrepareUnexpectedMessageFailsConn(t *testing.T) {
	cases := []dsCase{
		{"copy-out", [][]byte{dsParseOK, dsCopyOut, dsTail}, dsCopyWant},
		{"copy-in", [][]byte{dsCopyIn}, dsCopyWant},
		{"data-row", [][]byte{dsParseOK, dsDataRow0, dsRFQ}, "unexpected message"},
		{"ready-for-query-first", [][]byte{dsRFQ}, "ReadyForQuery"},
		{"ready-for-query-after-parse-complete", [][]byte{dsParseOK, dsRFQ}, "ReadyForQuery"},
		{"ready-for-query-before-row-description", [][]byte{dsParseOK, dsParamD0, dsRFQ}, "ReadyForQuery"},
		{"second-parse-complete", [][]byte{dsParseOK, dsParseOK, dsParamD0, dsNoData, dsRFQ}, "unexpected message"},
		{"command-complete", [][]byte{dsParseOK, dsParamD0, dsCmdCopy, dsRFQ}, "unexpected message"},
		{"close-complete", [][]byte{dsParseOK, dsCloseOK, dsRFQ}, "unexpected message"},
	}
	for _, tc := range cases {
		for _, split := range []bool{false, true} {
			name := tc.name
			if split {
				name += "/split"
			}
			t.Run(name, func(t *testing.T) {
				c := dsDial(t, func(c net.Conn) { dsWrite(c, split, tc.parts...) })
				ps, err := c.prepareStatement(dsCtx(t), "celst_1", "SELECT 1")
				dsCheckErr(t, err, tc.want)
				if ps != nil {
					t.Fatalf("a prepare that met an unexpected message returned a statement: %+v", ps)
				}
				dsAssertFailed(t, c)
			})
		}
	}
}

// The complete, in-phase prepare reply still succeeds, with its metadata.
func TestPrepareInPhaseStillSucceeds(t *testing.T) {
	for _, tc := range []struct {
		name  string
		parts [][]byte
		cols  int
	}{
		{"no-data", [][]byte{dsParseOK, dsParamD0, dsNoData, dsRFQ}, 0},
		{"row-description", [][]byte{dsParseOK, dsParamD0, dsRowDesc0, dsRFQ}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := dsDial(t, func(c net.Conn) { dsWrite(c, false, tc.parts...) })
			ps, err := c.prepareStatement(dsCtx(t), "celst_1", "SELECT 1")
			if err != nil {
				t.Fatal(err)
			}
			if ps == nil {
				t.Fatal("nil statement")
			}
			if c.closed.Load() {
				t.Fatal("the conn is closed after an in-phase prepare")
			}
		})
	}
}

// A server ErrorResponse is not a desync: the reply is complete and in step, so
// the caller gets the server's error and the conn lives, for each request kind.
func TestServerErrorStillLeavesConnUsable(t *testing.T) {
	errResp := dsFrame(protocol.BackendErrorResponse, []byte("SERROR\x00C42601\x00Msyntax error\x00\x00"))
	fatalResp := dsFrame(protocol.BackendErrorResponse, []byte("SFATAL\x00C57P01\x00Mshutting down\x00\x00"))
	for _, tc := range []struct {
		name  string
		parts [][]byte
		run   func(c *pgConn) error
	}{
		// A FATAL after an ERROR, before ReadyForQuery: the caller still gets the
		// first error.
		{"simple-fatal-after-error", [][]byte{errResp, fatalResp, dsRFQ}, func(c *pgConn) error {
			_, _, err := c.simpleExec(context.Background(), "BAD")
			return err
		}},
		{"extended-fatal-after-error", [][]byte{errResp, fatalResp, dsRFQ}, func(c *pgConn) error {
			_, err := c.extendedExec(context.Background(), "", "BAD", nil)
			return err
		}},
		{"prepare-fatal-after-error", [][]byte{errResp, fatalResp, dsRFQ}, func(c *pgConn) error {
			_, err := c.prepareStatement(context.Background(), "celst_1", "BAD")
			return err
		}},
		{"simple", [][]byte{errResp, dsRFQ}, func(c *pgConn) error {
			_, _, err := c.simpleExec(dsCtx(t), "BAD")
			return err
		}},
		{"extended", [][]byte{errResp, dsRFQ}, func(c *pgConn) error {
			_, err := c.extendedExec(dsCtx(t), "", "BAD", nil)
			return err
		}},
		{"extended-after-parse-complete", [][]byte{dsParseOK, errResp, dsRFQ}, func(c *pgConn) error {
			_, err := c.extendedExec(dsCtx(t), "", "BAD", nil)
			return err
		}},
		{"prepare", [][]byte{errResp, dsRFQ}, func(c *pgConn) error {
			_, err := c.prepareStatement(dsCtx(t), "celst_1", "BAD")
			return err
		}},
		{"prepare-after-parse-complete", [][]byte{dsParseOK, errResp, dsRFQ}, func(c *pgConn) error {
			_, err := c.prepareStatement(dsCtx(t), "celst_1", "BAD")
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := dsDial(t, func(c net.Conn) { dsWrite(c, false, tc.parts...) })
			err := tc.run(c)
			var pe *protocol.PGError
			if !errors.As(err, &pe) || pe.Code != "42601" {
				t.Fatalf("err = %v, want the server's 42601", err)
			}
			if c.closed.Load() {
				t.Fatal("a server ErrorResponse closed the conn")
			}
		})
	}
}

// After an ErrorResponse the server sends nothing but ReadyForQuery. A message
// in between is out of step too, whatever the request: the conn is failed
// instead of the rest of the reply being skipped to a ReadyForQuery that may
// belong to another request.
func TestMessageAfterErrorResponseFailsConn(t *testing.T) {
	errResp := dsFrame(protocol.BackendErrorResponse, []byte("SERROR\x00C42601\x00Msyntax error\x00\x00"))
	for _, tc := range []struct {
		name  string
		parts [][]byte
		run   func(t *testing.T, c *pgConn) error
	}{
		{"simple", [][]byte{errResp, dsDataRow0, dsRFQ}, func(t *testing.T, c *pgConn) error {
			_, _, err := c.simpleExec(dsCtx(t), "BAD")
			return err
		}},
		{"extended", [][]byte{errResp, dsDataRow0, dsRFQ}, func(t *testing.T, c *pgConn) error {
			_, err := c.extendedExec(dsCtx(t), "", "BAD", nil)
			return err
		}},
		{"extended-after-parse-complete", [][]byte{dsParseOK, errResp, dsCopyData, dsRFQ}, func(t *testing.T, c *pgConn) error {
			_, err := c.extendedExec(dsCtx(t), "", "BAD", nil)
			return err
		}},
		{"prepare", [][]byte{errResp, dsParamD0, dsRFQ}, func(t *testing.T, c *pgConn) error {
			_, err := c.prepareStatement(dsCtx(t), "celst_1", "BAD")
			return err
		}},
		{"copy-to", [][]byte{dsCopyOut, errResp, dsCopyData, dsRFQ}, func(t *testing.T, c *pgConn) error {
			return c.copyTo(dsCtx(t), "COPY t TO STDOUT", func([]byte) error { return nil })
		}},
	} {
		for _, split := range []bool{false, true} {
			name := tc.name
			if split {
				name += "/split"
			}
			t.Run(name, func(t *testing.T) {
				c := dsDial(t, func(c net.Conn) { dsWrite(c, split, tc.parts...) })
				err := tc.run(t, c)
				dsCheckErr(t, err, "")
				dsAssertFailed(t, c)
			})
		}
	}
}

// closePreparedServerAsync reuses the prepare request for Close S + Sync: its
// CloseComplete is expected, anything else fails the conn.
func TestClosePreparedReply(t *testing.T) {
	t.Run("close-complete-keeps-conn", func(t *testing.T) {
		c := dsDial(t, func(c net.Conn) { dsWrite(c, false, dsCloseOK, dsRFQ) })
		if err := c.closePreparedServerAsync("celst_1"); err != nil {
			t.Fatal(err)
		}
		dsEventually(t, func() bool { return c.pendingLen() == 0 })
		if c.closed.Load() {
			t.Fatal("a normal Close reply closed the conn")
		}
	})
	t.Run("stray-message-fails-conn", func(t *testing.T) {
		c := dsDial(t, func(c net.Conn) { dsWrite(c, false, dsCopyOut, dsTail) })
		if err := c.closePreparedServerAsync("celst_1"); err != nil {
			t.Fatal(err)
		}
		dsEventually(t, func() bool { return c.closed.Load() })
	})
	t.Run("ready-for-query-without-close-complete-fails-conn", func(t *testing.T) {
		c := dsDial(t, func(c net.Conn) { dsWrite(c, false, dsRFQ) })
		if err := c.closePreparedServerAsync("celst_1"); err != nil {
			t.Fatal(err)
		}
		dsEventually(t, func() bool { return c.closed.Load() })
	})
}

func (c *pgConn) pendingLen() int {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	return len(c.pending)
}

func dsEventually(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached within 3 s")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// CopyFrom and CopyTo have their own states: a message they do not expect fails
// the conn as well, with the error returned to the caller.
func TestCopyStatesUnexpectedMessageFailConn(t *testing.T) {
	t.Run("copy-to/row-description-after-copy-out-response", func(t *testing.T) {
		c := dsDial(t, func(c net.Conn) { dsWrite(c, false, dsCopyOut, dsRowDesc0, dsRFQ) })
		err := c.copyTo(dsCtx(t), "COPY t TO STDOUT", func([]byte) error { return nil })
		dsCheckErr(t, err, "unexpected message")
		dsAssertFailed(t, c)
	})
	t.Run("copy-to/second-copy-out-response", func(t *testing.T) {
		c := dsDial(t, func(c net.Conn) { dsWrite(c, false, dsCopyOut, dsCopyOut, dsRFQ) })
		err := c.copyTo(dsCtx(t), "COPY t TO STDOUT", func([]byte) error { return nil })
		dsCheckErr(t, err, "unexpected CopyOutResponse")
		dsAssertFailed(t, c)
	})
	t.Run("copy-to/ready-for-query-before-copy-done", func(t *testing.T) {
		c := dsDial(t, func(c net.Conn) { dsWrite(c, false, dsCopyOut, dsCopyData, dsRFQ) })
		err := c.copyTo(dsCtx(t), "COPY t TO STDOUT", func([]byte) error { return nil })
		dsCheckErr(t, err, "ReadyForQuery")
		dsAssertFailed(t, c)
	})
	t.Run("copy-to/not-a-copy", func(t *testing.T) {
		// CopyTo of a statement that is not a COPY: the server answers with a
		// result set.
		c := dsDial(t, func(c net.Conn) { dsWrite(c, false, dsRowDesc0, dsDataRow0, dsCmdCopy, dsRFQ) })
		err := c.copyTo(dsCtx(t), "SELECT 1", func([]byte) error { return nil })
		dsCheckErr(t, err, "unexpected message")
		dsAssertFailed(t, c)
	})
	t.Run("copy-from/row-description-after-copy-in-response", func(t *testing.T) {
		c := dsDial(t, func(c net.Conn) { dsWrite(c, false, dsCopyIn, dsRowDesc0) })
		_, err := c.copyFrom(dsCtx(t), "t", nil, CopyFromSlice([][]any{{"a"}, {"b"}}))
		if err == nil {
			t.Fatal("copyFrom returned no error")
		}
		dsEventually(t, func() bool { return c.closed.Load() })
	})
	t.Run("copy-from/ready-for-query-before-copy-in-response", func(t *testing.T) {
		c := dsDial(t, func(c net.Conn) { dsWrite(c, false, dsRFQ) })
		_, err := c.copyFrom(dsCtx(t), "t", nil, CopyFromSlice([][]any{{"a"}}))
		if err == nil {
			t.Fatal("copyFrom returned no error")
		}
		dsEventually(t, func() bool { return c.closed.Load() })
	})
}

// The startup handshake: a message it does not expect fails the dial (the conn
// is closed by the dialer), and the error is returned.
func TestStartupUnexpectedMessageFailsDial(t *testing.T) {
	for _, tc := range []struct {
		name  string
		parts [][]byte
	}{
		{"copy-out-response", [][]byte{dsFrame(protocol.BackendAuthentication, make([]byte, 4)), dsCopyOut}},
		{"ready-for-query-before-authentication-ok", [][]byte{dsRFQ}},
		{"command-complete", [][]byte{dsFrame(protocol.BackendAuthentication, make([]byte, 4)), dsCmdCopy}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addr := startFakePG(t, func(c net.Conn) {
				defer func() { _ = c.Close() }()
				if !readStartup(t, c) {
					return
				}
				_, _ = c.Write(dsFrames(tc.parts...))
				_, _ = io.Copy(io.Discard, c)
			})
			prov, err := eventloop.Resolve(nil)
			if err != nil {
				t.Fatal(err)
			}
			defer eventloop.Release(prov)
			host, port, _ := net.SplitHostPort(addr)
			dsn := DSN{Host: host, Port: port, User: "u", Options: Options{SSLMode: "disable"}}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			c, err := dialConn(ctx, prov, nil, dsn, 0)
			if err == nil {
				_ = c.Close()
				t.Fatal("dialConn succeeded against a server that sent a stray message during startup")
			}
		})
	}
}

// dsServer is a fake PG whose first connection answers its first request with
// bad (split: the head, a pause, the tail) and every other connection behaves:
// a Query is answered with "UPDATE <len(query)>", so a caller can tell its own
// reply from any other.
type dsServer struct {
	addr     string
	accepted atomic.Int32
	live     atomic.Int32
}

func startDSServer(t *testing.T, badConns int, bad func(c net.Conn)) *dsServer {
	t.Helper()
	s := &dsServer{}
	s.addr = startFakePG(t, func(c net.Conn) {
		n := int(s.accepted.Add(1))
		s.live.Add(1)
		defer s.live.Add(-1)
		fakePGTrustStartup(t, c, 1, 2, func(c net.Conn) {
			for {
				typ, body, err := readMsg(c)
				if err != nil || typ == protocol.MsgTerminate {
					return
				}
				if typ != protocol.MsgQuery {
					continue
				}
				if n <= badConns {
					bad(c)
					continue
				}
				q := strings.TrimRight(string(body), "\x00")
				_ = writeCommandComplete(c, "UPDATE "+itoa(len(q)))
				_ = writeReadyForQuery(c, 'I')
			}
		})
	})
	return s
}

func itoa(n int) string {
	var b [20]byte
	i := len(b)
	for n >= 10 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	i--
	b[i] = byte('0' + n)
	return string(b[i:])
}

// Pool accounting after a conn is failed by a misused COPY: the pool redials, no
// slot leaks (with MaxOpen 1 and 2), the next callers get their own reply, never
// an error that belongs to the misused statement, and the number of server
// connections stays bounded.
func TestPoolAfterDesyncFailAll(t *testing.T) {
	const iters = 30
	for _, mode := range []string{"direct", "loop"} {
		for _, maxOpen := range []int{1, 2} {
			t.Run(mode+"/maxopen"+itoa(maxOpen), func(t *testing.T) {
				// Every connection answers its COPY statement with a CopyOut head
				// and, 30 ms later, the tail; the pool keeps dialing replacements.
				s := startDSServer(t, 1<<30, func(c net.Conn) {
					dsWrite(c, true, dsCopyOut, dsTail)
				})
				opts := []Option{WithMaxOpen(maxOpen)}
				if mode == "loop" {
					opts = append(opts, WithEngine(looptest.Unstarted()))
				}
				p, err := Open("postgres://u@"+s.addr+"/d?sslmode=disable&statement_cache_size=0", opts...)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = p.Close() }()
				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				defer cancel()
				for i := 0; i < iters; i++ {
					_, err := p.ExecContext(ctx, "COPY t TO STDOUT")
					if err == nil || !strings.Contains(err.Error(), "CopyTo") || !strings.Contains(err.Error(), "CopyFrom") {
						t.Fatalf("iteration %d: COPY through ExecContext: err = %v, want one naming CopyFrom and CopyTo", i, err)
					}
					// The very next call, at once (before the tail of the reply
					// could arrive): it must not be handed any of it. It goes to
					// a new conn, which answers a COPY statement the same way, so
					// it gets the same named error; what it must never get is an
					// error about a message it did not expect.
					for k := 0; k < 3; k++ {
						_, err := p.ExecContext(ctx, "COPY t TO STDOUT"+strings.Repeat(" ", k))
						if err == nil || strings.Contains(err.Error(), "unexpected message") ||
							strings.Contains(err.Error(), "ReadyForQuery") || !strings.Contains(err.Error(), "CopyTo") {
							t.Fatalf("iteration %d follower %d: err = %v", i, k, err)
						}
					}
				}
				st := p.Stats()
				if st.InUse != 0 {
					t.Fatalf("slots leaked: Stats = %+v after every call returned", st)
				}
				if st.Open > maxOpen {
					t.Fatalf("Open = %d > MaxOpen %d", st.Open, maxOpen)
				}
				if got := int(s.accepted.Load()); got <= 1 {
					t.Fatalf("the pool never redialed (accepted %d)", got)
				}
				dsEventually(t, func() bool { return int(s.live.Load()) <= maxOpen })
			})
		}
	}
}

// With one bad conn only: after it is failed, the pool serves a healthy
// replacement and the callers get their own replies.
func TestPoolRecoversAfterDesync(t *testing.T) {
	for _, mode := range []string{"direct", "loop"} {
		for _, maxOpen := range []int{1, 2} {
			t.Run(mode+"/maxopen"+itoa(maxOpen), func(t *testing.T) {
				s := startDSServer(t, 1, func(c net.Conn) { dsWrite(c, true, dsCopyOut, dsTail) })
				opts := []Option{WithMaxOpen(maxOpen)}
				if mode == "loop" {
					opts = append(opts, WithEngine(looptest.Unstarted()))
				}
				p, err := Open("postgres://u@"+s.addr+"/d?sslmode=disable&statement_cache_size=0", opts...)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = p.Close() }()
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				if _, err := p.ExecContext(ctx, "COPY t TO STDOUT"); err == nil {
					t.Fatal("the COPY through ExecContext returned no error")
				}
				for k := 0; k < 5; k++ {
					q := "SELECT 1" + strings.Repeat(" ", k)
					res, err := p.ExecContext(ctx, q)
					if err != nil {
						t.Fatalf("follower %d: %v", k, err)
					}
					n, _ := res.RowsAffected()
					if int(n) != len(q) {
						t.Fatalf("follower %d got another request's reply: RowsAffected = %d, want %d", k, n, len(q))
					}
				}
				if st := p.Stats(); st.InUse != 0 || st.Open > maxOpen {
					t.Fatalf("Stats = %+v, MaxOpen %d", st, maxOpen)
				}
				dsEventually(t, func() bool { return int(s.live.Load()) <= maxOpen })
			})
		}
	}
}

// Messages for a request that arrive while its caller is still writing it. The
// request is built completely before it is queued, so the loop's first Handle
// on it (which reads SkipParse) races with nothing: a server that streams stray
// messages the moment the conn is up gives the loop work at every point of the
// caller's enqueue and write. Run with -race.
func TestExtendedRequestIsCompleteBeforeItIsQueued(t *testing.T) {
	var stop atomic.Bool
	t.Cleanup(func() { stop.Store(true) })
	addr := startFakePG(t, func(c net.Conn) {
		fakePGTrustStartup(t, c, 1, 2, func(c net.Conn) {
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _ = io.Copy(io.Discard, c)
			}()
			stray := dsFrames(dsCopyData)
			for i := 0; i < 400 && !stop.Load(); i++ {
				if _, err := c.Write(stray); err != nil {
					break
				}
				time.Sleep(20 * time.Microsecond)
			}
			_ = c.Close()
			wg.Wait()
		})
	})
	prov, err := eventloop.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer eventloop.Release(prov)
	host, port, _ := net.SplitHostPort(addr)
	dsn := DSN{Host: host, Port: port, User: "u", Options: Options{SSLMode: "disable", StatementCacheSize: 8}}
	for conn := 0; conn < 20; conn++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		c, err := dialConn(ctx, prov, nil, dsn, 0)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		for i := 0; i < 200 && !c.closed.Load(); i++ {
			// A named statement: SkipParse is true.
			_, _ = c.extendedExec(ctx, "celst_1", "SELECT 1", nil)
		}
		cancel()
		_ = c.Close()
	}
}

// Both rules on one simple-query reply (the merge of the multi-result-set check
// with the desync rule): a multi-statement reply that is in step but returns rows
// from two statements is the multi-result-set error and the conn lives; the same
// reply with a message that does not belong in it fails the conn.
func TestMultiResultSetsAndDesyncOnOneConn(t *testing.T) {
	cmd := func(tag string) []byte { return dsFrame(protocol.BackendCommandComplete, []byte(tag+"\x00")) }
	run := func(t *testing.T, c *pgConn) error {
		rows, err := c.simpleQuery(dsCtx(t), "SELECT 1; SELECT 2")
		if err != nil {
			return err
		}
		dest := make([]driver.Value, len(rows.Columns()))
		for err = nil; err == nil; err = rows.Next(dest) { //revive:disable-line:empty-block
		}
		_ = rows.Close()
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	t.Run("in-step-two-sets-keeps-conn", func(t *testing.T) {
		c := dsDial(t, func(c net.Conn) {
			dsWrite(c, false, dsRowDesc0, cmd("SELECT 0"), dsRowDesc0, cmd("SELECT 0"), dsRFQ)
		})
		err := run(t, c)
		if err == nil || !strings.Contains(err.Error(), "result set") {
			t.Fatalf("err = %v, want the multi-result-set error", err)
		}
		if c.closed.Load() {
			t.Fatal("an in-step multi-statement reply closed the conn")
		}
	})
	for _, split := range []bool{false, true} {
		name := "unexpected-message-between-sets-fails-conn"
		if split {
			name += "/split"
		}
		t.Run(name, func(t *testing.T) {
			c := dsDial(t, func(c net.Conn) {
				dsWrite(c, split, dsRowDesc0, cmd("SELECT 0"), dsParseOK, dsRowDesc0, cmd("SELECT 0"), dsRFQ)
			})
			err := run(t, c)
			dsCheckErr(t, err, "unexpected message")
			dsAssertFailed(t, c)
		})
		t.Run("ready-for-query-inside-second-set-fails-conn", func(t *testing.T) {
			c := dsDial(t, func(c net.Conn) {
				dsWrite(c, split, dsRowDesc0, cmd("SELECT 0"), dsRowDesc0, dsRFQ)
			})
			err := run(t, c)
			dsCheckErr(t, err, "ReadyForQuery")
			dsAssertFailed(t, c)
		})
	}
}
