//go:build linux

package websocket

import (
	"fmt"
	"io"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestServerErrsJudgesResets is a table test of serverErrs, the verdict on
// every server-side error. A read reset (ECONNRESET) is judged like any other
// read error, and is excused only when the client gave up and closed first.
// Times are rig times; closeNs 0 means the client had not closed.
func TestServerErrsJudgesResets(t *testing.T) {
	const port = 40001
	ms := int64(time.Millisecond)
	cases := []struct {
		name    string
		joined  bool  // the server's connection was joined to a client
		gaveUp  bool  // the client gave up on the connection
		closeNs int64 // when the client closed its socket, 0 while open
		errNs   int64 // when the server noted the reset
		excused bool  // want the reset excused; otherwise judged, with reason in its line
		reason  string
	}{
		{"reset after the client gave up is excused", true, true, 100 * ms, 150 * ms, true, ""},
		{"reset before the client closed is judged", true, true, 200 * ms, 150 * ms, false, "BEFORE its client closed"},
		{"reset while the client is open is judged", true, false, 0, 150 * ms, false, "had not closed"},
		{"reset after a clean close is judged", true, false, 100 * ms, 150 * ms, false, "after its client closed"},
		{"reset on a connection never joined is judged", false, false, 0, 150 * ms, false, "never joined"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := &wsoRig{t0: time.Now()}
			s := &wsoSrv{g: g, port: port}
			// The reset enters as a handler's does: through noteReadErr.
			if k := s.noteReadErr(syscall.ECONNRESET); k != srvReadReset {
				t.Fatalf("noteReadErr(ECONNRESET) = %d, want srvReadReset", k)
			}
			if len(s.errs) != 1 {
				t.Fatalf("the reset was recorded %d times, want once", len(s.errs))
			}
			// noteErr stamps the rig clock; the case sets its own time.
			s.errs[0].ns = tc.errNs
			g.hs = []*wsoSrv{s}
			if tc.joined {
				g.clis = []*wsoCli{{port: port, gaveUp: tc.gaveUp, closeNs: tc.closeNs}}
			}
			judged, excused := g.serverErrs()
			if tc.excused {
				if len(judged) != 0 || len(excused) != 1 {
					t.Fatalf("judged=%d excused=%d, want the reset excused", len(judged), len(excused))
				}
				return
			}
			if len(judged) != 1 || len(excused) != 0 {
				t.Fatalf("judged=%d excused=%d, want one judged line", len(judged), len(excused))
			}
			if judged[0].kind != "read" || !strings.Contains(judged[0].line, tc.reason) {
				t.Fatalf("judged line %q, want kind read and %q in it", judged[0].line, tc.reason)
			}
		})
	}
}

// TestNoteReadErrClassifies pins noteReadErr, which the handlers of the
// inbound and pause-cancel oracles call for every read error that is not a
// CloseError. A reset and any other error must reach serverErrs (they are
// noted); a close, EOF or closed connection must not be. isCloseErr counts
// ECONNRESET as a close, so the table includes a reset, bare and wrapped,
// that a classifier calling isCloseErr first would lose.
func TestNoteReadErrClassifies(t *testing.T) {
	cases := []struct {
		name  string
		err   error
		kind  srvReadKind
		noted int // errors recorded for serverErrs
	}{
		{"reset", syscall.ECONNRESET, srvReadReset, 1},
		{"wrapped reset", fmt.Errorf("read tcp: %w", syscall.ECONNRESET), srvReadReset, 1},
		{"EOF is a close", io.EOF, srvReadClose, 0},
		{"closed connection is a close", ErrClosed, srvReadClose, 0},
		{"a protocol error is another error", ErrProtocol, srvReadOther, 1},
		{"a truncated frame is another error", io.ErrUnexpectedEOF, srvReadOther, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := &wsoRig{t0: time.Now()}
			s := &wsoSrv{g: g, port: 40002}
			if k := s.noteReadErr(tc.err); k != tc.kind {
				t.Fatalf("noteReadErr(%v) = %d, want %d", tc.err, k, tc.kind)
			}
			if len(s.errs) != tc.noted {
				t.Fatalf("noteReadErr(%v) recorded %d errors, want %d", tc.err, len(s.errs), tc.noted)
			}
		})
	}
}
