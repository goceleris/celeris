//go:build linux

package iouring

import (
	"context"
	"testing"

	"golang.org/x/sys/unix"
)

// celeris#884 item 57: with the development-only CELERIS_IOURING_FIXED_FILES=1
// (ACCEPT_DIRECT) a connection's slot in w.conns is its fixed-file index, not a
// descriptor number. RegisterConn's "already an HTTP connection" check
// (connSlotBusy, and the worker's re-check in armDriverRecv) compared a real
// descriptor with that index, and refused a driver for a number that is only a
// slot in use. A fixed-file connection has no descriptor in the process, so
// its slot is not a number a driver can collide with.
//
// The conn is installed by hand: the fixed-file accept path is incomplete
// (celeris#541) and not something a test can drive an HTTP client through.

func fixedSlotConn884(fd int, fixed bool) *connState {
	cs := acquireConnState(context.Background(), fd, 0, false)
	cs.fixedFile = fixed
	return cs
}

func TestRegisterConnAgainstFixedFileSlots884(t *testing.T) {
	for _, tc := range []struct {
		name    string
		fixed   bool
		refused bool
	}{
		{"fixed-file-slot-is-not-a-descriptor", true, false},
		// The control: an HTTP connection on that very number is still refused.
		{"http-conn-on-the-number-is-refused", false, true},
	} {
		t.Run(tc.name+"/api-check", func(t *testing.T) {
			e, stop := startTestEngine(t)
			t.Cleanup(stop)
			w, wl := e.workers[0], e.WorkerLoop(0)
			d := newDriverFD884(t)
			w.setConnSlot(d.fd, fixedSlotConn884(d.fd, tc.fixed))
			t.Cleanup(func() { w.clearConnSlot(d.fd) })
			err := wl.RegisterConn(d.fd, func([]byte) {}, func(err error) { d.closed <- err })
			if tc.refused {
				if err == nil {
					t.Fatalf("RegisterConn(%d) on an HTTP connection's number succeeded", d.fd)
				}
				return
			}
			if err != nil {
				t.Fatalf("celeris#884 item 57: RegisterConn(%d) refused for the number of a fixed-file slot: %v", d.fd, err)
			}
			settleDriverRecv(t, w, d.fd)
			d.unregisterAndWait(t, wl)
		})
		// The worker's own check, when the slot is taken after RegisterConn's.
		t.Run(tc.name+"/worker-check", func(t *testing.T) {
			e, stop := startTestEngine(t)
			t.Cleanup(stop)
			w, wl := e.workers[0], e.WorkerLoop(0)
			p := newWorkerPark(t, w, wl)
			d := newDriverFD884(t)
			p.park()
			if err := wl.RegisterConn(d.fd, func([]byte) {}, func(err error) { d.closed <- err }); err != nil {
				t.Fatalf("apparatus: RegisterConn(%d) with the slot empty: %v", d.fd, err)
			}
			p.onWorker(func() { w.setConnSlot(d.fd, fixedSlotConn884(d.fd, tc.fixed)) })
			t.Cleanup(func() { w.clearConnSlot(d.fd) })
			p.release()
			if tc.refused {
				if fired, err := d.waitClosed(); !fired || err == nil {
					t.Fatalf("the worker did not refuse a driver on an HTTP connection's number: onClose fired=%v err=%v", fired, err)
				}
				_ = unix.Close(d.fd)
				d.closePeer()
				p.d.unregisterAndWait(t, wl)
				return
			}
			submitRoundTrip(t, w) // the worker has applied the register action
			select {
			case err := <-d.closed:
				t.Fatalf("celeris#884 item 57: the worker refused the driver for the number of a fixed-file slot: onClose(%v)", err)
			default:
			}
			settleDriverRecv(t, w, d.fd)
			d.unregisterAndWait(t, wl)
			p.d.unregisterAndWait(t, wl)
		})
	}
}

func newDriverFD884(t *testing.T) *testDriver {
	t.Helper()
	d := &testDriver{closed: make(chan error, 1), recv: make(chan struct{}, 16)}
	d.fd, d.peer = nonblockSocketPair(t)
	return d
}
