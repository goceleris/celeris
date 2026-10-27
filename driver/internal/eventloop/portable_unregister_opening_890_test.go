package eventloop

// celeris#890 (found by the correctness review of PR #1051): an
// UnregisterConn(fd) that ran while RegisterConn(fd, ...) was still opening
// the net.Conn was lost. register had put a placeholder in the table;
// UnregisterConn found it, deleted it and returned nil; register's second
// section checked only the worker's closed flag, so it installed the opened
// conn and started its reader anyway: the conn stayed in the table, onClose
// never fired and the net.Conn was never closed.
//
// The two outcomes the contract allows are tested (the call is linearizable):
// either UnregisterConn ran before the registration (the number is unknown to
// it: engine.ErrUnknownFD, as on the Linux workers, whose RegisterConn also
// inserts the conn atomically) and the registration stands, fully live; or it
// ran after, returned nil and the conn is fully torn down (onClose fired, the
// net.Conn is closed, the table is clean). Never a nil return with the conn
// left registered. No build tag: the portable worker is compiled everywhere.

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/engine"
)

func TestUnregisterConnWhileRegisterConnIsOpening890(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pw := newPortableWorker(ctx, 0)
	w := &pw
	t.Cleanup(func() { _ = w.shutdown() })

	const fd = 4242
	ours, theirs := net.Pipe()
	t.Cleanup(func() { _ = theirs.Close(); _ = ours.Close() })

	opening := make(chan struct{})
	release := make(chan struct{})
	open := func() (net.Conn, error) {
		close(opening)
		<-release
		return ours, nil
	}
	var closes atomic.Int32
	closed := make(chan error, 4)
	onClose := func(err error) {
		closes.Add(1)
		closed <- err
	}

	type regResult struct {
		fd  int
		err error
	}
	regDone := make(chan regResult, 1)
	go func() {
		got, err := w.register(fd, false, open, func([]byte) {}, onClose)
		regDone <- regResult{got, err}
	}()
	<-opening

	unregErr := w.UnregisterConn(fd)
	close(release)
	reg := <-regDone

	switch {
	case unregErr == nil:
		// UnregisterConn took the registration down: onClose must fire, the
		// net.Conn must be closed and the table must not hold the number.
		select {
		case err := <-closed:
			if err != nil {
				t.Errorf("onClose(%v), want nil after UnregisterConn", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("UnregisterConn returned nil during the open, but the conn was left registered: onClose never fired (register returned fd %d, err %v)", reg.fd, reg.err)
		}
		_ = theirs.SetReadDeadline(time.Now().Add(2 * time.Second))
		if _, err := theirs.Read(make([]byte, 1)); err == nil || errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("the net.Conn is still open after UnregisterConn returned nil: peer read err = %v", err)
		}
		w.mu.RLock()
		_, still := w.conns[fd]
		w.mu.RUnlock()
		if still {
			t.Errorf("the table still holds fd %d after UnregisterConn returned nil", fd)
		}
	case errors.Is(unregErr, engine.ErrUnknownFD):
		// UnregisterConn ran before the registration existed: it did nothing,
		// and the registration must be intact.
		if reg.err != nil {
			t.Fatalf("register failed after an UnregisterConn that reported an unknown fd: %v", reg.err)
		}
		if closes.Load() != 0 {
			t.Fatalf("onClose fired %d times for a registration that was never unregistered", closes.Load())
		}
		if err := w.UnregisterConn(fd); err != nil {
			t.Fatalf("second UnregisterConn: %v", err)
		}
		select {
		case <-closed:
		case <-time.After(2 * time.Second):
			t.Fatal("the registration did not tear down on its own UnregisterConn: onClose never fired")
		}
	default:
		t.Fatalf("UnregisterConn during the open returned %v", unregErr)
	}
	if got := closes.Load(); got != 1 {
		t.Errorf("onClose fired %d times, want 1", got)
	}
}
