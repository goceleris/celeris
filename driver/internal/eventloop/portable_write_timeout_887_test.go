//go:build !windows

package eventloop

// celeris#887 item 19, with a peer that really stops reading. Not built on
// windows: its loopback sockets accepted a 64 MiB write without a reader (see
// portable_blocked_write_test.go), so no Write blocks there.

import (
	"errors"
	"net"
	"os"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/engine"
)

// c887Stuck registers a client conn whose server never reads and returns the id
// and the recorder; the caller issues the big Write.
func c887Stuck(t *testing.T, l *Loop) (id int, rec *recorder) {
	t.Helper()
	srv := newLoopbackServer(t)
	client, _ := srv.dial(t) // the server side never reads
	if tc, ok := client.(*net.TCPConn); ok {
		_ = tc.SetWriteBuffer(4 << 10)
	}
	rec = newRecorder()
	id, err := registrar(t, l, 0).RegisterNetConn(client, rec.onRecv, rec.onClose)
	if err != nil {
		t.Fatalf("RegisterNetConn: %v", err)
	}
	return id, rec
}

func TestPortableWriteToAPeerThatStoppedReadingTimesOut887(t *testing.T) {
	SetPortableWriteTimeout(400 * time.Millisecond)
	t.Cleanup(func() { SetPortableWriteTimeout(0) })
	l := newPortableTestLoop(t, 1)
	id, rec := c887Stuck(t, l)

	start := time.Now()
	werr := make(chan error, 1)
	go func() { werr <- l.WorkerLoop(0).Write(id, make([]byte, 64<<20)) }()
	var err error
	select {
	case err = <-werr:
	case <-time.After(portableWait):
		t.Fatal("a Write to a peer that never reads was still blocked after 5 s with a 400 ms write timeout")
	}
	if took := time.Since(start); took < 300*time.Millisecond {
		t.Errorf("the Write failed after %v, before its 400 ms bound: the peer was reading, the test did not drive the bound", took)
	}
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("Write = %v, want a deadline error", err)
	}
	// The torn stream is closed, not left for the next message.
	if cerr := rec.waitClose(t); !errors.Is(cerr, os.ErrDeadlineExceeded) {
		t.Errorf("onClose(%v), want the Write's deadline error", cerr)
	}
	if err := l.WorkerLoop(0).Write(id, []byte("x")); !errors.Is(err, engine.ErrUnknownFD) {
		t.Errorf("Write after the timeout = %v, want ErrUnknownFD: the conn must be closed", err)
	}
}

func TestPortableWriteWithTheBoundRemovedStillBlocksUntilClose887(t *testing.T) {
	SetPortableWriteTimeout(-1)
	t.Cleanup(func() { SetPortableWriteTimeout(0) })
	l := newPortableTestLoop(t, 1)
	id, _ := c887Stuck(t, l)
	werr := make(chan error, 1)
	go func() { werr <- l.WorkerLoop(0).Write(id, make([]byte, 64<<20)) }()
	select {
	case err := <-werr:
		t.Fatalf("a Write with no bound returned (%v) while the peer was not reading", err)
	case <-time.After(1500 * time.Millisecond):
	}
	_ = l.Close()
	select {
	case err := <-werr:
		if err == nil {
			t.Error("the blocked Write returned nil after Loop.Close")
		}
	case <-time.After(portableWait):
		t.Fatal("Loop.Close did not release the Write")
	}
}

func TestPortableWriteTimeoutSettings887(t *testing.T) {
	t.Cleanup(func() { SetPortableWriteTimeout(0) })
	for _, tc := range []struct {
		set, want time.Duration
	}{
		{0, DefaultPortableWriteTimeout},
		{2 * time.Second, 2 * time.Second},
		{-time.Second, 0},
		{0, DefaultPortableWriteTimeout},
	} {
		SetPortableWriteTimeout(tc.set)
		if got := currentPortableWriteTimeout(); got != tc.want {
			t.Errorf("after SetPortableWriteTimeout(%v) the bound is %v, want %v", tc.set, got, tc.want)
		}
	}
	if DefaultPortableWriteTimeout != 30*time.Second {
		t.Errorf("default bound %v, want 30 s", DefaultPortableWriteTimeout)
	}
}
