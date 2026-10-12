//go:build unix

package postgres

// celeris#887 item 8: Close of a conn that a loop has already torn down
// writes Terminate and unregisters by number (celeris#859 needs both calls),
// and both find the conn gone: the loop answers ErrUnknownFD. That is what
// teardown looks like, not a failure of Close; database/sql logs or surfaces
// the error of every conn it closes. Any other error still is one.

import (
	"errors"
	"io"
	"testing"

	"github.com/goceleris/celeris/internal/engine"
)

func TestPgCloseOfATornDownConnIgnoresUnknownFD887(t *testing.T) {
	for _, tc := range []struct {
		name               string
		writeErr, unregErr error
		want               error // nil: Close returns nil
	}{
		{"both unknown", engine.ErrUnknownFD, engine.ErrUnknownFD, nil},
		{"terminate write unknown", engine.ErrUnknownFD, nil, nil},
		{"unregister unknown", nil, engine.ErrUnknownFD, nil},
		{"wrapped unknown", errors.Join(errors.New("write"), engine.ErrUnknownFD), engine.ErrUnknownFD, nil},
		// Only ErrUnknownFD is ignored: another error from either call is
		// still Close's result, the first one if both fail.
		{"terminate write fails", io.ErrClosedPipe, engine.ErrUnknownFD, io.ErrClosedPipe},
		{"unregister fails", engine.ErrUnknownFD, io.ErrShortWrite, io.ErrShortWrite},
		{"both fail, the write first", io.ErrClosedPipe, io.ErrShortWrite, io.ErrClosedPipe},
	} {
		t.Run(tc.name, func(t *testing.T) {
			loop := newC859Loop()
			loop.writeErr, loop.unregErr = tc.writeErr, tc.unregErr
			c, _, ino := newC859Conn(t, loop)
			n := c.fd
			c.onClose(io.EOF) // the loop tore the conn down

			err := c.Close()
			if tc.want == nil && err != nil {
				t.Errorf("Close = %v, want nil: the conn was already torn down", err)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Errorf("Close = %v, want %v", err, tc.want)
			}
			// Ignoring the error must not skip the calls: #859 needs both, on
			// the conn's own number.
			ops := map[string]bool{}
			for _, call := range loop.snapshot() {
				ops[call.op] = true
			}
			if !ops["Write"] || !ops["UnregisterConn"] {
				t.Errorf("Close made the calls %v, want Write (Terminate) and UnregisterConn", ops)
			}
			checkCalls(t, loop, n, ino)
		})
	}
}
