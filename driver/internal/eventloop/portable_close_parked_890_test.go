package eventloop

// celeris#890 (the #996 round-3 test gap): Loop.Close on the portable worker
// must not run a conn's onClose while the conn's reader is inside onRecv. The
// order is part of the contract: onClose is the last callback of a conn
// (portable_order_test.go), so it fires on the conn's reader goroutine once
// the reader has seen the close, never on the goroutine that called
// Loop.Close. TestPortableLoopCloseOrder only observes that by racing a peer
// that sends without pause, and a revert slips through it; this test parks
// the reader inside onRecv, so the order is not a race. It has no build tag:
// it runs wherever the portable worker is compiled (Linux, macOS, windows).

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPortableLoopCloseWaitsForAParkedOnRecv890(t *testing.T) {
	srv := newLoopbackServer(t)
	l := newPortableTestLoop(t, 1)
	client, server := srv.dial(t)

	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	letGo := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(letGo)
	var parked atomic.Bool // true while onRecv is inside its park
	var firedWhileParked atomic.Bool
	var onRecvReturned atomic.Bool
	var firedAfterReturn atomic.Bool
	var fireCount atomic.Int32
	gotErr := make(chan error, 2)
	var enterOnce sync.Once
	onRecv := func([]byte) {
		enterOnce.Do(func() {
			parked.Store(true)
			close(entered)
			<-release
			parked.Store(false)
			onRecvReturned.Store(true)
		})
	}
	onClose := func(err error) {
		fireCount.Add(1)
		if parked.Load() {
			firedWhileParked.Store(true)
		}
		if onRecvReturned.Load() {
			firedAfterReturn.Store(true)
		}
		gotErr <- err
	}
	if _, err := registrar(t, l, 0).RegisterNetConn(client, onRecv, onClose); err != nil {
		t.Fatalf("RegisterNetConn: %v", err)
	}
	if _, err := server.Write([]byte("x")); err != nil {
		t.Fatalf("server write: %v", err)
	}
	select {
	case <-entered:
	case <-time.After(portableWait):
		t.Fatal("onRecv never ran")
	}

	closed := make(chan error, 1)
	go func() { closed <- l.Close() }()

	// While the reader is parked in onRecv, Loop.Close has torn the conn down
	// (the net.Conn is closed) but may neither fire onClose nor return: it
	// waits for the reader. A window in which nothing happens proves nothing
	// by itself, so it is only the negative half; the positive half is below.
	select {
	case err := <-gotErr:
		t.Fatalf("onClose(%v) fired while the conn's reader was parked inside onRecv: Loop.Close ran it on its own goroutine", err)
	case err := <-closed:
		t.Fatalf("Loop.Close returned (%v) while the conn's reader was parked inside onRecv", err)
	case <-time.After(300 * time.Millisecond):
	}
	if !connClosed(client) {
		t.Error("Loop.Close did not close the net.Conn while onRecv was parked")
	}

	letGo()
	select {
	case err := <-closed:
		if err != nil {
			t.Errorf("Loop.Close: %v", err)
		}
	case <-time.After(portableWait):
		t.Fatal("Loop.Close did not return within 5 s of onRecv's return")
	}
	// Loop.Close returns after the readers are done, so onClose has returned.
	if n := fireCount.Load(); n != 1 {
		t.Errorf("onClose fired %d times, want once, by the time Loop.Close returned", n)
	}
	select {
	case err := <-gotErr:
		if !errors.Is(err, ErrLoopClosed) {
			t.Errorf("onClose(%v), want ErrLoopClosed", err)
		}
	default:
		t.Error("onClose had not fired when Loop.Close returned")
	}
	if firedWhileParked.Load() {
		t.Error("onClose ran while onRecv was parked")
	}
	if !firedAfterReturn.Load() {
		t.Error("onClose did not run after onRecv returned")
	}
}
