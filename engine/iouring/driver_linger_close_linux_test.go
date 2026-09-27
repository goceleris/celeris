//go:build linux

package iouring

import (
	"errors"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// celeris#735: since celeris#691 the engine holds its own duplicate of every
// driver socket (driverConn.opFD) and closes it when it finalizes the conn.
// After UnregisterConn and the caller's own close that duplicate is the
// socket's last reference, so its close(2) does the socket's release, and
// with SO_LINGER set and data unsent to a peer that is not reading, the
// release waits up to the linger time for the FIN's ACK. The engine closed it
// on the LockOSThread'd worker goroutine, so every other connection of the
// ring waited as long: no CQE processed, no accept, no flush.
//
// The rig is the issue's probe: a victim driver conn V on the same worker as
// a lingering socket L; L is unregistered and closed, and 100 ms later V's
// peer writes a byte. Measured on the unfixed engine: 2.9 s from the write to
// V's onRecv with a 3 s linger, against tens of microseconds without one.

// lingerStallBudget is the most V's byte may take to reach onRecv. A worker
// that is not blocked delivers it in microseconds; a blocked one holds it for
// the rest of the linger (lingerStallSecs less the 100 ms the test waits
// first). The gap absorbs a loaded -race run.
const (
	lingerStallSecs   = 3
	lingerStallBudget = time.Second
)

// socketIdentity is what a descriptor names: its device and inode.
type socketIdentity struct{ dev, ino uint64 }

func identityOf(t *testing.T, fd int) socketIdentity {
	t.Helper()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		t.Fatalf("fstat(%d): %v", fd, err)
	}
	return socketIdentity{uint64(st.Dev), uint64(st.Ino)}
}

// stillNames reports whether fd is open and still names the file id.
func stillNames(fd int, id socketIdentity) bool {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return false
	}
	return uint64(st.Dev) == id.dev && uint64(st.Ino) == id.ino
}

// lingeringDriver registers a lingering socket on w, settles its RECV, and
// returns it with its onClose channel, the engine's duplicate of it and what
// that duplicate names.
func lingeringDriver(t *testing.T, w *Worker, linger int) (fd int, drain func(), closed chan error, opFD int, id socketIdentity) {
	t.Helper()
	fd, drain = lingeringTCPSocket(t, linger)
	if linger == 0 {
		// The control: SO_LINGER off, so the close returns at once.
		if err := unix.SetsockoptLinger(fd, unix.SOL_SOCKET, unix.SO_LINGER, &unix.Linger{Onoff: 0}); err != nil {
			t.Fatalf("clear SO_LINGER: %v", err)
		}
	}
	id = identityOf(t, fd)
	closed = make(chan error, 1)
	if err := w.RegisterConn(fd, func([]byte) {}, func(err error) { closed <- err }); err != nil {
		t.Fatalf("RegisterConn(lingering): %v", err)
	}
	settleDriverRecv(t, w, fd)
	dc := driverConnOf(w, fd)
	if dc == nil {
		t.Fatal("the lingering conn is not registered")
	}
	return fd, drain, closed, dc.opFD, id
}

// waitFinalized waits until the worker has finalized fd's conn: it has left
// the driver map, so the close of the engine's duplicate has begun.
func waitFinalized(t *testing.T, w *Worker, fd int) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); driverConnOf(w, fd) != nil; {
		if time.Now().After(deadline) {
			t.Fatalf("fd %d was never finalized after UnregisterConn", fd)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestDriverLingeringCloseDoesNotStallTheWorker is the issue's measurement as
// a test. The linger arm fails on the unfixed engine; the no-linger arm is
// the control that shows the rig measures a worker that is not blocked.
func TestDriverLingeringCloseDoesNotStallTheWorker(t *testing.T) {
	for _, tc := range []struct {
		name   string
		linger int
	}{{"no_linger_control", 0}, {"linger", lingerStallSecs}} {
		t.Run(tc.name, func(t *testing.T) {
			e, stop := startTestEngine(t)
			defer stop()
			w, wl := e.workers[0], e.WorkerLoop(0)
			v := registerSettledDriver(t, w, wl)
			defer v.unregisterAndWait(t, wl)

			fd, drain, closed, opFD, id := lingeringDriver(t, w, tc.linger)
			drained := false
			defer func() {
				if !drained {
					drain()
				}
			}()
			if err := wl.UnregisterConn(fd); err != nil {
				t.Fatalf("UnregisterConn: %v", err)
			}
			_ = unix.Close(fd)
			waitFinalized(t, w, fd)
			// The issue's probe: give the worker the cancel and the start of
			// the close before V's peer writes.
			time.Sleep(100 * time.Millisecond)

			start := time.Now()
			if _, err := unix.Write(v.peer, []byte{'v'}); err != nil {
				t.Fatalf("write V's peer: %v", err)
			}
			var lat time.Duration
			select {
			case <-v.recv:
				lat = time.Since(start)
			case <-time.After(10 * time.Second):
				lat = -1
			}
			var closedEarly bool
			select {
			case err := <-closed:
				closed <- err // put it back for the check below
				closedEarly = true
			default:
			}
			t.Logf("celeris735 LINGER arm=%s linger=%ds v_onrecv_ms=%.3f onclose_before_v=%v",
				tc.name, tc.linger, float64(lat)/1e6, closedEarly)
			if lat < 0 || lat > lingerStallBudget {
				t.Errorf("celeris#735: V's byte reached onRecv after %v (budget %v) while another conn's "+
					"close lingered (SO_LINGER %d s): the worker was blocked in close(2)", lat, lingerStallBudget, tc.linger)
			}
			if tc.linger > 0 && closedEarly {
				t.Errorf("L's onClose fired while its close still lingered: a driver must see the " +
					"socket closed before onClose (engine/provider.go)")
			}

			// The peer drains, the close returns, and then onClose fires,
			// once, with the unregister's nil error, the socket closed.
			drain()
			drained = true
			select {
			case err := <-closed:
				if err != nil {
					t.Errorf("onClose(%v), want nil after UnregisterConn", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("L's onClose never fired")
			}
			if stillNames(opFD, id) {
				t.Errorf("onClose fired with the engine's duplicate (fd %d) still open on the socket", opFD)
			}
		})
	}
}

// TestDriverShutdownWaitsForAHandedOffClose: a conn finalized before the
// engine stops has its close handed off the worker; the engine stops while
// that close lingers. Shutdown must wait for it and still fire the conn's
// onClose, with the conn's own nil error, not errEngineShutdown and not
// never: the loop that would have fired it has stopped (celeris#735).
func TestDriverShutdownWaitsForAHandedOffClose(t *testing.T) {
	e, stop := startTestEngine(t)
	t.Cleanup(stop) // idempotent; the test calls it itself below
	w, wl := e.workers[0], e.WorkerLoop(0)
	fd, drain, closed, opFD, id := lingeringDriver(t, w, 2)
	if err := wl.UnregisterConn(fd); err != nil {
		t.Fatalf("UnregisterConn: %v", err)
	}
	_ = unix.Close(fd)
	waitFinalized(t, w, fd)

	stopped := make(chan struct{})
	go func() { stop(); close(stopped) }()
	// Let the stop reach the worker while the close still lingers, then let
	// the close finish.
	time.Sleep(200 * time.Millisecond)
	drain()
	select {
	case <-stopped:
	case <-time.After(15 * time.Second):
		t.Fatal("the engine did not stop")
	}
	select {
	case err := <-closed:
		if errors.Is(err, errEngineShutdown) || err != nil {
			t.Errorf("onClose(%v), want nil: the conn was unregistered before the shutdown", err)
		}
	default:
		t.Error("the engine stopped without firing the onClose of a conn whose close it had handed off")
	}
	if stillNames(opFD, id) {
		t.Errorf("the engine stopped with its duplicate (fd %d) of the socket still open", opFD)
	}
}
