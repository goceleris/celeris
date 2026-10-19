//go:build linux

package iouring

// celeris#884 item 54: Worker.Write on a conn that UnregisterConn (or a
// failure) has put in its closing state used to return nil and drop the
// bytes, while the epoll engine and the standalone loop return
// engine.ErrUnknownFD for a number they no longer know. A driver whose caller
// races its own Close (redis PubSub.Subscribe against PubSub.Close) saw
// success on io_uring although nothing was sent.

import (
	"errors"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/engine"
)

// TestDriverWriteAfterUnregisterReturnsErrUnknownFD884 parks the worker, so
// the unregister is queued and applied only after the release: the conn is
// still a key of the driver map, in its closing state, when Write is called,
// which is the window the closing branch of Write answers for. Once it is
// finalized (the map no longer has it) Write returns ErrUnknownFD by the
// lookup, which the test also checks, so the two answers agree.
func TestDriverWriteAfterUnregisterReturnsErrUnknownFD884(t *testing.T) {
	e, stop := startTestEngine(t)
	t.Cleanup(stop)
	w, wl := e.workers[0], e.WorkerLoop(0)

	a := registerSettledDriver(t, w, wl)
	defer a.closePeer()
	p := newWorkerPark(t, w, wl)

	p.park()
	if err := wl.UnregisterConn(a.fd); err != nil {
		t.Fatalf("UnregisterConn: %v", err)
	}
	dc := driverConnOf(w, a.fd)
	if dc == nil {
		t.Fatal("apparatus: the conn left the driver map while the worker was parked")
	}
	dc.mu.Lock()
	closing := dc.closing
	dc.mu.Unlock()
	if !closing {
		t.Fatal("apparatus: the conn is not in its closing state after UnregisterConn")
	}
	if err := wl.Write(a.fd, []byte("late")); !errors.Is(err, engine.ErrUnknownFD) {
		t.Errorf("Write on a closing conn = %v, want engine.ErrUnknownFD (the epoll engine's answer for a number it no longer knows)", err)
	}
	_ = unix.Close(a.fd)
	p.release()

	// The refused bytes were not queued: the peer reads EOF, not "late".
	a.expectReleased(t, "UnregisterConn, then Write while the worker was busy")
	waitFinalized(t, w, a.fd)
	if err := wl.Write(a.fd, []byte("later")); !errors.Is(err, engine.ErrUnknownFD) {
		t.Errorf("Write on a finalized conn = %v, want engine.ErrUnknownFD", err)
	}
	p.d.unregisterAndWait(t, wl)
}
