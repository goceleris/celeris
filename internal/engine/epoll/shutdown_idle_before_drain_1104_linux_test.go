//go:build linux

package epoll

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/conn"
	"github.com/goceleris/celeris/internal/engine"
)

// celeris#1104 (the flakes lane; TestShutdownPoolWaitGateRequestsInProgressAnd
// IdleConns885 failed once in CI with an idle keep-alive conn that had no FIN
// 1.5 s into the shutdown). A loop with no HTTP/2 conn has nothing to wait for
// (h2PoolSettled is true at once), so it never raises the pool wait's gate and
// goes straight into shutdown(): phase 2b is a send drain that polls every conn
// with a response still queued until its socket takes it, up to the budget of
// the Shutdown (20 s there), and phase 3, which closes the conns, comes after.
// A client that is slow to read one download so held the FIN of every idle
// keep-alive conn on the same loop for as long as the drain lasted, where the
// loops that have an HTTP/2 conn close theirs 250 ms in (the gate), and where
// net/http's Shutdown closes its idle conns at once.

// idleConn1104 adds an HTTP/1 conn that holds nothing (a keep-alive conn after
// its response) to the loop of r, and returns the peer's descriptor.
func (r *drainRig874) idleConn1104(t *testing.T) (fd, peer int) {
	t.Helper()
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_NONBLOCK, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	t.Cleanup(func() { _ = unix.Close(pair[1]) })
	if pair[0] >= len(r.l.conns) {
		t.Fatalf("socketpair fd %d outside the test conn table", pair[0])
	}
	cs := acquireConnState(t.Context(), pair[0], 4096, false)
	cs.protocol = engine.HTTP1
	cs.detected = true
	cs.h1State = conn.NewH1State()
	cs.remoteAddr = "127.0.0.1:10"
	r.l.conns[pair[0]] = cs
	r.l.addLiveConn(cs)
	r.l.connCount++
	r.l.activeConns.Add(1)
	return pair[0], pair[1]
}

func TestShutdownClosesAnIdleConnWhileAnotherConnsSendDrainWaits1104(t *testing.T) {
	const queued = 2 << 20
	r := newDrainRig874(t, queued) // a conn whose peer reads nothing
	_, idlePeer := r.idleConn1104(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	var budget atomic.Pointer[context.Context]
	c := ctx
	budget.Store(&c)
	r.l.drainBudget = &budget

	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		r.l.shutdown()
	}()

	// The idle conn's FIN, with the other conn's peer still not reading.
	begin := time.Now()
	fds := []unix.PollFd{{Fd: int32(idlePeer), Events: unix.POLLIN}}
	var fin bool
	for time.Since(begin) < 1500*time.Millisecond && !fin {
		if n, _ := unix.Poll(fds, 50); n > 0 {
			var b [1]byte
			if m, err := unix.Read(idlePeer, b[:]); m == 0 && err == nil {
				fin = true
			}
		}
	}
	took := time.Since(begin).Round(time.Millisecond)
	// Let the drain finish: the stalled conn's peer reads now.
	r.startReader()
	select {
	case <-shutdownDone:
	case <-time.After(20 * time.Second):
		t.Fatal("shutdown did not return once the stalled conn's peer read")
	}
	r.wait(t)
	if !fin {
		t.Fatalf("the idle conn had no FIN %v into the shutdown, while the send drain of another conn of the loop waited for its peer (celeris#1104)", took)
	}
	t.Logf("the idle conn had its FIN %v into the shutdown", took)
}

// The pass runs ahead of phase 1, on conns that have served a request: their
// stream and Context are pooled (CloseH1).
//
//   - sync: a conn the loop owns is torn down once by closeConn: counted closed
//     once, OnDisconnect once, a FIN.
//   - promoted: a conn with a dispatch goroutine parked after its request, as
//     production makes one (asyncPromoted is set whenever the goroutine is
//     started: the flag is sticky, so every conn that ever served an Async route
//     is one). The pass closes it as it closes the sync one, from outside the
//     goroutine (celeris#1165): once, with one OnDisconnect, and the goroutine
//     exits on the asyncClosed closeConn sets.
func TestShutdownIdlePassTearsAConnThatServedARequestDownOnce1104(t *testing.T) {
	for _, promoted := range []bool{false, true} {
		async := promoted
		name := "sync"
		if promoted {
			name = "promoted"
		}
		t.Run(name, func(t *testing.T) {
			var disconnects atomic.Int64
			l := shutdownLoop863(t, func(string) { disconnects.Add(1) })
			l.async = async
			pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_NONBLOCK, 0)
			if err != nil {
				t.Fatalf("socketpair: %v", err)
			}
			fd, peer := pair[0], pair[1]
			t.Cleanup(func() { _ = unix.Close(peer) })
			if fd >= len(l.conns) {
				t.Fatalf("socketpair fd %d outside the test conn table", fd)
			}
			cs := acquireConnState(t.Context(), fd, 4096, async)
			cs.protocol = engine.HTTP1
			cs.detected = true
			cs.remoteAddr = "127.0.0.1:9"
			cs.writeFn = l.makeWriteFn(cs)
			l.conns[fd] = cs
			l.addLiveConn(cs)
			l.connCount++
			l.activeConns.Add(1)
			l.initProtocol(cs)
			const req = "GET /x HTTP/1.1\r\nHost: x\r\n\r\n"
			if async {
				// A dispatch goroutine answers the request and parks, as the loop
				// starts one: with the conn marked promoted and counted.
				cs.asyncPromoted = true
				l.asyncPromoted = &atomic.Uint64{} // the cumulative promotion counter
				l.asyncPromoted.Add(1)
				cs.asyncInMu.Lock()
				cs.asyncInBuf = append(cs.asyncInBuf, req...)
				cs.asyncRun = true
				cs.asyncInMu.Unlock()
				l.asyncWG.Add(1)
				go l.runAsyncHandler(cs)
				for dl := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
					cs.asyncInMu.Lock()
					parked := cs.asyncParked && cs.asyncRun && len(cs.asyncInBuf) == 0
					cs.asyncInMu.Unlock()
					if parked {
						break
					}
					if time.Now().After(dl) {
						t.Fatal("the dispatch goroutine never parked after its request")
					}
				}
			} else {
				if _, err := unix.Write(peer, []byte(req)); err != nil {
					t.Fatalf("write request: %v", err)
				}
				l.drainRead(fd, time.Now().UnixNano())
				if l.conns[fd] == nil {
					t.Fatal("the conn was closed by its own request")
				}
			}
			// The response went out; the peer takes it, so the send queue is empty.
			buf := make([]byte, 4096)
			for dl := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
				if n, err := unix.Read(peer, buf); err == nil && n > 0 {
					break
				}
				if time.Now().After(dl) {
					t.Fatal("no response")
				}
			}
			if promoted {
				// The pass alone closes the parked promoted conn (celeris#1165).
				if n := l.closeIdleH1(false, time.Now().UnixNano()); n != 1 {
					t.Errorf("the pass closed %d promoted conns, want 1", n)
				}
				if l.conns[fd] != nil || l.activeConns.Load() != 0 || disconnects.Load() != 1 {
					t.Errorf("after the pass the promoted conn is live=%v activeConns=%d OnDisconnect=%d, want closed, 0, 1",
						l.conns[fd] != nil, l.activeConns.Load(), disconnects.Load())
				}
			}
			done := make(chan struct{})
			go func() { defer close(done); l.shutdown() }()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("shutdown did not return")
			}
			wantCalls, wantActive := int64(1), int64(0)
			if got := disconnects.Load(); got != wantCalls {
				t.Errorf("OnDisconnect ran %d times for one conn, want %d", got, wantCalls)
			}
			if got := l.closeCount.Load(); got != uint64(wantCalls) {
				t.Errorf("closeCount %d for one conn, want %d", got, wantCalls)
			}
			if got := l.activeConns.Load(); got != wantActive {
				t.Errorf("activeConns %d after shutdown, want %d", got, wantActive)
			}
			if n, err := unix.Read(peer, make([]byte, 1)); n != 0 || err != nil {
				t.Errorf("the peer read n=%d err=%v after shutdown, want a FIN (0, nil)", n, err)
			}
		})
	}
}
