//go:build linux

package iouring

import (
	"errors"
	"net"
	"runtime"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/deferlinger"
)

// celeris#884 item 71 (PR #1020 review r2): no test checked that a worker whose
// goroutine ended without returning has left the SO_REUSEPORT group BEFORE it
// is marked exited and the pause notified (reportWorkerExit), nor that
// leaveGroupAfterExit closes the ring. The tests of worker_exit_pause_lifecycle
// pass if the shutdown(SHUT_RD) and the close move after the flag and the
// notify, because abnormalWorkerExits is counted first and a pause's own
// wait hides the rest. The epoll half of the item (stopAccepting against the
// flags and the notify) is left to that engine's lane.

// TestLeaveGroupAfterExitClosesTheRingAndTheListener884 is the unit half: after
// leaveGroupAfterExit the listen socket is out of the group (a connect is
// refused, which the shutdown(SHUT_RD) alone brings about at once, before any
// close), its descriptor and the ring's are closed, and the worker says so
// (listenFDClosed, which PauseAccept waits on). Red with the ring's Close
// removed, or with the listener's.
func TestLeaveGroupAfterExitClosesTheRingAndTheListener884(t *testing.T) {
	ring, err := NewRing(64, 0, 0)
	if err != nil {
		skipOrFail656(t, "io_uring unavailable: %v", err)
	}
	ringFD := ring.fd
	lfd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.SetsockoptInt(lfd, unix.SOL_SOCKET, unix.SO_REUSEPORT, 1); err != nil {
		t.Fatal(err)
	}
	if err := unix.Bind(lfd, &unix.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatal(err)
	}
	if err := unix.Listen(lfd, 8); err != nil {
		t.Fatal(err)
	}
	sa, _ := unix.Getsockname(lfd)
	addr := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: sa.(*unix.SockaddrInet4).Port}
	t.Cleanup(func() {
		if fdOpen704(lfd) {
			_ = unix.Close(lfd)
		}
		if fdOpen704(ringFD) {
			_ = ring.Close()
		}
	})
	// The control: the listener takes a connection before.
	if c, err := net.DialTimeout("tcp", addr.String(), time.Second); err != nil {
		t.Fatalf("setup: the listener refused a connection: %v", err)
	} else {
		_ = c.Close()
	}

	w := &Worker{id: 3, ring: ring, listenFD: lfd, handoffLoss: &handoffLossStats{}}
	w.leaveGroupAfterExit()

	if _, err := net.DialTimeout("tcp", addr.String(), time.Second); !errors.Is(err, syscall.ECONNREFUSED) {
		t.Errorf("a connect after leaveGroupAfterExit gave %v, want connection refused: the listener is still in the group", err)
	}
	if w.ring != nil || fdOpen704(ringFD) {
		t.Errorf("the ring is not closed after leaveGroupAfterExit: w.ring=%v fd open=%v", w.ring != nil, fdOpen704(ringFD))
	}
	if w.listenFD != -1 || fdOpen704(lfd) {
		t.Errorf("the listener is not closed after leaveGroupAfterExit: listenFD=%d fd open=%v", w.listenFD, fdOpen704(lfd))
	}
	if !w.listenFDClosed.Load() {
		t.Error("listenFDClosed is not set after leaveGroupAfterExit: a pause would wait for it")
	}
}

// (Not a guard on its own: under the order mutant it was red 3 runs of 5, the
// watcher racing the worker; TestReportWorkerExitLeavesTheGroupBeforeTheFlag884
// below is the deterministic detector, red 5 of 5.)
// TestGoexitedWorkerLeavesTheGroupBeforeItIsMarkedExited884 is the engine half,
// in the child process every test that kills a worker runs in. A watcher per
// worker spins on exited; at the instant it sees it set, the listener must
// already be closed (listenFDClosed is stored by leaveGroupAfterExit after its
// shutdown(SHUT_RD) and its closes), which is what lets PauseAccept, woken by
// the notify that follows the flag, promise the group has shed the worker. Red
// with reportWorkerExit's leaveGroupAfterExit moved after its exited.Store.
func TestGoexitedWorkerLeavesTheGroupBeforeItIsMarkedExited884(t *testing.T) {
	if !child848(t) {
		return
	}
	e, _, cancel, done := start848(t, "127.0.0.1:0", true)
	addr := e.Addr().String()
	e.mu.Lock()
	ws := append([]*Worker(nil), e.workers...)
	e.mu.Unlock()

	type seen struct {
		w            *Worker
		listenClosed bool
		at           time.Time
	}
	results := make(chan seen, len(ws))
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for _, w := range ws {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !w.exited.Load() {
				select {
				case <-stop:
					return
				default:
				}
				runtime.Gosched()
			}
			results <- seen{w, w.listenFDClosed.Load(), time.Now()}
		}()
	}
	// The worker that serves this connection runs the handler that calls
	// runtime.Goexit.
	boom, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = boom.Close() }()
	_, _ = boom.Write([]byte("GET /boom HTTP/1.1\r\nHost: x\r\n\r\n"))
	var got seen
	select {
	case got = <-results:
	case <-time.After(5 * time.Second):
		close(stop)
		t.Fatal("PREMISE: no worker was marked exited within 5s of the Goexit")
	}
	close(stop)
	wg.Wait()
	if !got.listenClosed {
		t.Errorf("worker %d was seen marked exited with its listener not closed: the worker left the SO_REUSEPORT group after the flag and the notify a PauseAccept waits on, not before", got.w.id)
	}
	if e.abnormalWorkerExits.Load() != 1 {
		t.Errorf("PREMISE: abnormalWorkerExits = %d, want 1", e.abnormalWorkerExits.Load())
	}
	stop848(t, cancel, done)
	t.Logf("celeris848 RESULT iouring-exit-order worker=%d listener_closed_at_exited=%v", got.w.id, got.listenClosed)
}

// TestReportWorkerExitLeavesTheGroupBeforeTheFlag884 is the same order at the
// function, many times over (a watcher that sees the flag set between the flag
// and the end of the group's shedding is a race the engine arm catches once in
// a few runs): reportWorkerExit on a worker holding a listening socket, with a
// goroutine spinning on exited; whenever it sees the flag, listenFDClosed, which
// leaveGroupAfterExit stores after its shutdown(SHUT_RD) and its closes, must
// be set; and a goroutine told by the pause's channel (the notify) must find it
// set too. Red with leaveGroupAfterExit moved after the flag and the notify.
func TestReportWorkerExitLeavesTheGroupBeforeTheFlag884(t *testing.T) {
	const rounds = 400
	e := &Engine{}
	early := 0
	for round := range rounds {
		lfd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := unix.Bind(lfd, &unix.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
			_ = unix.Close(lfd)
			t.Fatal(err)
		}
		if err := unix.Listen(lfd, 1); err != nil {
			_ = unix.Close(lfd)
			t.Fatal(err)
		}
		w := &Worker{id: round, listenFD: lfd, handoffLoss: &handoffLossStats{}, pause: &deferlinger.PauseState{}}
		changed := w.pause.Changed()
		saw := make(chan bool, 1)
		go func() {
			for !w.exited.Load() {
				runtime.Gosched()
			}
			saw <- w.listenFDClosed.Load()
		}()
		notified := make(chan bool, 1)
		go func() {
			<-changed
			notified <- w.listenFDClosed.Load()
		}()
		e.reportWorkerExit(w)
		if !<-saw {
			early++
		}
		if !<-notified {
			early++
		}
		if fdOpen704(lfd) {
			_ = unix.Close(lfd)
			t.Fatalf("round %d: reportWorkerExit returned with the listener open", round)
		}
	}
	if early > 0 {
		t.Errorf("%d of %d observations (the flag, the notify) found the worker's listener not yet closed: the worker is marked exited and the pause notified before it has left the SO_REUSEPORT group", early, 2*rounds)
	}
}
