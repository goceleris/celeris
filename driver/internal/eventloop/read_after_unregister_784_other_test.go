//go:build !linux && !windows

package eventloop

// celeris#784 on the non-Linux fallback loop (loop_other.go). Its reader
// goroutine reads through its own duplicate of the fd, so it cannot read a
// reused number. But when the reader sees EOF it fires onClose and then
// removes the conn's map entry BY NUMBER: if onClose led the owner to
// unregister and close the fd and a new conn registered the same number in
// the meantime, the removal took the new conn's entry. This test parks the
// reader in onClose to hold it there.
//
// CI builds this file on macOS but runs no tests there (ci.yml "Build"); the
// test runs on a non-Linux developer host.

import (
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func c784OtherPair(t *testing.T) (int, int) {
	t.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	for _, fd := range fds {
		if err := unix.SetNonblock(fd, true); err != nil {
			t.Fatalf("nonblock: %v", err)
		}
	}
	return fds[0], fds[1]
}

func TestReaderEOFSparesTheConnThatTakesTheNumber784(t *testing.T) {
	l, err := New(1)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	w := l.WorkerLoop(0).(*worker)

	a, aPeer := c784OtherPair(t)
	entered := make(chan error, 1)
	release := make(chan struct{})
	released := false
	t.Cleanup(func() {
		if !released {
			close(release)
		}
	})
	if err := w.RegisterConn(a, func([]byte) {}, func(err error) {
		entered <- err
		<-release
	}); err != nil {
		t.Fatalf("RegisterConn(A): %v", err)
	}
	// A's peer goes away: A's reader reads EOF and fires onClose.
	_ = unix.Close(aPeer)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("A's onClose never fired after its peer closed")
	}

	// The owner reacts: unregister, close, and the number is reused by B,
	// which registers on the same worker.
	if err := w.UnregisterConn(a); err != nil {
		t.Fatalf("UnregisterConn(A): %v", err)
	}
	if err := unix.Close(a); err != nil {
		t.Fatalf("close A: %v", err)
	}
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	b, bPeer := pair[0], pair[1]
	if b != a {
		if err := unix.Dup2(b, a); err != nil {
			t.Fatalf("dup2 onto %d: %v", a, err)
		}
		_ = unix.Close(b)
		b = a
	}
	_ = unix.SetNonblock(b, true)
	_ = unix.SetNonblock(bPeer, true)
	t.Cleanup(func() { _ = unix.Close(b); _ = unix.Close(bPeer) })
	if err := w.RegisterConn(b, func([]byte) {}, func(error) {}); err != nil {
		t.Fatalf("RegisterConn(B) on A's number: %v", err)
	}
	t.Cleanup(func() { _ = w.UnregisterConn(b) })
	bConn := func() *driverConn {
		w.mu.RLock()
		defer w.mu.RUnlock()
		return w.conns[b]
	}
	registeredB := bConn()

	released = true
	close(release)
	// A's reader removes its entry right after onClose returns. Give it a
	// second to do so (it takes microseconds), watching B's entry.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && bConn() == registeredB {
		time.Sleep(time.Millisecond)
	}
	gone := bConn() != registeredB
	werr := w.Write(b, []byte("hi"))
	t.Logf("C784 other: B's entry gone after A's reader finished: %v; Write(B) err %v", gone, werr)
	if gone {
		t.Errorf("A's reader removed B's map entry: it removed the entry of A's number after A was unregistered and B registered the number")
	}
	if werr != nil {
		t.Errorf("Write(B): %v, want nil", werr)
	}
}
