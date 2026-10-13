//go:build linux

package epoll

// celeris#885 item 18: the HTTP-to-HTTP twin of celeris#771. An HTTP conn's
// epoll events carried Pad 0, so an event the worker had collected for a conn
// that was closed earlier in the same batch was dispatched, by descriptor
// number, to whatever conn had taken the number since. Every HTTP epoll_event
// now carries the generation of the conn it was registered for (bit 31 set, so
// it is never 0 and never a driver conn's), and the run loop drops an event
// whose generation is not the conn's in the slot.
//
// TestStaleHTTPEventDoesNotReachConnOnReusedNumber885 is the probe, on a
// running engine. TestHTTPEventsCarryAGeneration885 reads the events the
// kernel hands back, site by site, from a loop that is not running: the
// registrations are made by the loop's own functions on the test goroutine,
// which stands in for the loop thread.

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/resource"
)

// httpTagged885 reports whether pad has the bit that marks an HTTP conn's
// generation: set for every HTTP conn, clear for a driver conn's.
func httpTagged885(pad int32) bool { return uint32(pad)&0x80000000 != 0 }

// slotOf885 reads l.conns[fd] the way a reader off the loop thread does.
func slotOf885(l *Loop, fd int) *connState {
	l.driverMu.RLock()
	defer l.driverMu.RUnlock()
	if fd < 0 || fd >= len(l.conns) {
		return nil
	}
	return l.conns[fd]
}

// eventFor885 returns the epoll_event the kernel reports for fd on l's epoll
// set, waiting up to d. Events of other descriptors are discarded.
func eventFor885(t *testing.T, l *Loop, fd int, d time.Duration) (unix.EpollEvent, bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	evs := make([]unix.EpollEvent, 16)
	for {
		n, err := unix.EpollWait(l.epollFD, evs, 20)
		if err != nil && err != unix.EINTR {
			t.Fatalf("epoll_wait: %v", err)
		}
		for i := 0; i < n; i++ {
			if int(evs[i].Fd) == fd {
				return evs[i], true
			}
		}
		if !time.Now().Before(deadline) {
			return unix.EpollEvent{}, false
		}
	}
}

// drainEvents885 discards what is ready on l's epoll set now.
func drainEvents885(l *Loop) {
	evs := make([]unix.EpollEvent, 16)
	for {
		n, err := unix.EpollWait(l.epollFD, evs, 0)
		if n <= 0 || (err != nil && err != unix.EINTR) {
			return
		}
	}
}

// adoptedPair885 adopts one end of a socketpair onto l as an HTTP conn
// (attachAdoptedFD, on this goroutine) and returns its descriptor, its peer
// and its slot. The peer has written a byte before the adoption, so the ADD
// reports EPOLLIN at once.
func adoptedPair885(t *testing.T, l *Loop) (fd, peer int, cs *connState) {
	t.Helper()
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	fd, peer = pair[0], pair[1]
	if fd >= len(l.conns) {
		t.Fatalf("socketpair fd %d outside the test conn table", fd)
	}
	t.Cleanup(func() { _ = unix.Close(peer) })
	if _, err := unix.Write(peer, []byte("x")); err != nil {
		t.Fatalf("write the peer: %v", err)
	}
	l.attachAdoptedFD(context.Background(), fd, engine.Carryover{RemoteAddr: "pair"}, time.Now().UnixNano())
	cs = slotOf885(l, fd)
	if cs == nil {
		t.Fatalf("attachAdoptedFD(%d) left the slot empty", fd)
	}
	t.Cleanup(func() { l.closeConn(fd) })
	return fd, peer, cs
}

// wantTagged885 checks that the event the kernel handed back for a site
// carries the conn's generation: tagged, and the one the connState keeps.
func wantTagged885(t *testing.T, l *Loop, site string, ev unix.EpollEvent, ok bool) {
	t.Helper()
	if !ok {
		t.Fatalf("%s: the kernel reported no event for the conn", site)
	}
	if !httpTagged885(ev.Pad) {
		t.Errorf("%s: the event the kernel hands back has Pad=%#x; an HTTP conn's events must carry its generation (bit 31 set)", site, uint32(ev.Pad))
		return
	}
	cs := slotOf885(l, int(ev.Fd))
	if cs == nil || cs.gen != uint32(ev.Pad) {
		var got uint32
		if cs != nil {
			got = cs.gen
		}
		t.Errorf("%s: the event carries generation %#x, the conn's is %#x", site, uint32(ev.Pad), got)
	}
}

// TestHTTPEventsCarryAGeneration885: every site that registers or modifies an
// HTTP conn's interest hands the kernel the conn's generation, so every event
// it reports carries it. A MOD replaces the whole event data: a site that
// omits the generation makes the conn's later events untagged again.
func TestHTTPEventsCarryAGeneration885(t *testing.T) {
	t.Run("attachAdoptedFD ADD", func(t *testing.T) {
		l := newLedgerLoop(t)
		l.async = true
		fd, _, _ := adoptedPair885(t, l)
		ev, ok := eventFor885(t, l, fd, time.Second)
		wantTagged885(t, l, "attachAdoptedFD's EPOLL_CTL_ADD", ev, ok)
	})

	t.Run("acceptAll ADD", func(t *testing.T) {
		l := newLedgerLoop(t)
		lfd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, 0)
		if err != nil {
			t.Fatalf("socket: %v", err)
		}
		t.Cleanup(func() { _ = unix.Close(lfd) })
		if err := unix.Bind(lfd, &unix.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
			t.Fatalf("bind: %v", err)
		}
		if err := unix.Listen(lfd, 8); err != nil {
			t.Fatalf("listen: %v", err)
		}
		sa, _ := unix.Getsockname(lfd)
		port := sa.(*unix.SockaddrInet4).Port
		l.listenFD = lfd
		c, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", itoa885(port)))
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		t.Cleanup(func() { _ = c.Close() })
		// The handshake completes in the kernel; accept4 sees the conn once it
		// is queued.
		var fd int
		for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
			l.acceptAll(context.Background(), time.Now().UnixNano())
			for i, cs := range l.conns {
				if cs != nil {
					fd = i
				}
			}
			if fd != 0 {
				break
			}
		}
		if fd == 0 {
			t.Fatal("acceptAll accepted nothing")
		}
		t.Cleanup(func() { l.closeConn(fd) })
		if _, err := c.Write([]byte("x")); err != nil {
			t.Fatalf("write: %v", err)
		}
		ev, ok := eventFor885(t, l, fd, time.Second)
		wantTagged885(t, l, "acceptAll's EPOLL_CTL_ADD", ev, ok)
	})

	t.Run("modEpollOut arm and disarm", func(t *testing.T) {
		for _, async := range []bool{false, true} {
			name := "sync conn"
			if async {
				name = "async conn (the driverMu branch)"
			}
			t.Run(name, func(t *testing.T) {
				l := newLedgerLoop(t)
				l.async = async
				fd, _, cs := adoptedPair885(t, l)
				drainEvents885(l)
				// Arm: the socket is writable, so the MOD reports EPOLLOUT at once.
				l.armEpollOut(cs)
				if !cs.epollOut {
					t.Fatal("armEpollOut did not arm EPOLLOUT")
				}
				ev, ok := eventFor885(t, l, fd, time.Second)
				if ok && ev.Events&unix.EPOLLOUT == 0 {
					t.Fatalf("the arming MOD reported %#x, want EPOLLOUT in it", ev.Events)
				}
				wantTagged885(t, l, "armEpollOut's EPOLL_CTL_MOD", ev, ok)
				// Disarm: the byte the peer wrote is still unread, so the MOD
				// reports EPOLLIN at once.
				l.disarmEpollOut(cs)
				ev, ok = eventFor885(t, l, fd, time.Second)
				wantTagged885(t, l, "disarmEpollOut's EPOLL_CTL_MOD", ev, ok)
			})
		}
	})

	// The pause and the resume of a detached conn's reads. A MOD with no
	// events still reports EPOLLHUP, which is what these read back: the peer
	// closes after the pause.
	t.Run("drainDetachQueue pause and resume", func(t *testing.T) {
		l := newLedgerLoop(t)
		l.async = true
		fd, peer, cs := adoptedPair885(t, l)
		drainEvents885(l)

		cs.recvPauseDesired.Store(true)
		l.enqueueDetach(cs)
		l.drainDetachQueue()
		if !cs.recvPaused {
			t.Fatal("drainDetachQueue did not apply the pause")
		}
		drainEvents885(l)
		_ = unix.Close(peer)
		ev, ok := eventFor885(t, l, fd, time.Second)
		wantTagged885(t, l, "drainDetachQueue's pause EPOLL_CTL_MOD", ev, ok)

		cs.recvPauseDesired.Store(false)
		l.enqueueDetach(cs)
		l.drainDetachQueue()
		if cs.recvPaused {
			t.Fatal("drainDetachQueue did not apply the resume")
		}
		ev, ok = eventFor885(t, l, fd, time.Second)
		wantTagged885(t, l, "drainDetachQueue's resume EPOLL_CTL_MOD", ev, ok)
	})

	// drainRead's inline pause: a detached conn whose middleware asked for a
	// pause while the request was handled.
	t.Run("drainRead recv pause", func(t *testing.T) {
		l := newLedgerLoop(t)
		l.async = true
		l.handler = &asyncReuseHandler{}
		fd, peer, cs := adoptedPair885(t, l)
		drainEvents885(l)
		// adoptedPair885 wrote "x" to detect and consume; replace it with a
		// request, so the read parses, and ask for the pause.
		var b [8]byte
		_, _ = unix.Read(fd, b[:])
		if _, err := unix.Write(peer, []byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
			t.Fatalf("write the request: %v", err)
		}
		cs.recvPauseDesired.Store(true)
		l.drainRead(fd, time.Now().UnixNano())
		if !cs.recvPaused {
			t.Fatalf("drainRead did not apply the pause (conn closed: %v)", slotOf885(l, fd) == nil)
		}
		drainEvents885(l)
		_ = unix.Close(peer)
		ev, ok := eventFor885(t, l, fd, time.Second)
		wantTagged885(t, l, "drainRead's pause EPOLL_CTL_MOD", ev, ok)
	})
}

func itoa885(n int) string {
	var b [12]byte
	i := len(b)
	for {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
		if n == 0 {
			break
		}
	}
	return string(b[i:])
}

// startHTTPEngine885 starts a two-worker epoll engine answering "ok" to every
// request, and returns it with a stop func.
func startHTTPEngine885(t *testing.T) (*Engine, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	e, err := New(resource.Config{
		Addr:      addr,
		Protocol:  engine.HTTP1,
		Resources: resource.Resources{Workers: 2},
	}, &asyncReuseHandler{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { _ = e.Listen(ctx); close(done) }()
	deadline := time.Now().Add(15 * time.Second)
	for e.Addr() == nil && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if e.Addr() == nil || e.NumWorkers() < 2 {
		cancel()
		<-done
		t.Fatal("engine did not bind")
	}
	var once sync.Once
	return e, func() { once.Do(func() { cancel(); <-done }) }
}

// adoptOn885 hands fd to loop l as an HTTP conn, the way AdoptConn does for
// the loop it picks, and waits until the loop has installed it.
func adoptOn885(t *testing.T, l *Loop, fd int) *connState {
	t.Helper()
	l.adoptQMu.Lock()
	l.adoptQueue = append(l.adoptQueue, adoptItem{fd: fd, carry: engine.Carryover{RemoteAddr: "adopted"}})
	l.adoptQPending.Store(1)
	l.wakeFD.Signal()
	l.adoptQMu.Unlock()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		if cs := slotOf885(l, fd); cs != nil {
			return cs
		}
	}
	t.Fatalf("loop %d never installed adopted fd %d", l.id, fd)
	return nil
}

// TestStaleHTTPEventDoesNotReachConnOnReusedNumber885 is the probe of
// celeris#885 item 18, the HTTP twin of TestStaleEventDoesNotReachConnOnReusedNumber771.
//
// The worker is held in a driver conn Q's onRecv while the next epoll_wait's
// batch builds: P's event, then HTTP conn F's (its peer closed: EPOLLIN and
// EPOLLRDHUP). Q released, P's onRecv runs on the loop thread, as a sweep
// that reaps F or a transplant would: it closes F, puts another HTTP conn G on
// F's number, and returns. F's event, collected before the close, is then
// dispatched, and it must not close G nor hide G's bytes.
func TestStaleHTTPEventDoesNotReachConnOnReusedNumber885(t *testing.T) {
	eng, stop := startHTTPEngine885(t)
	t.Cleanup(stop)
	l := eng.loops[0]
	wl := eng.WorkerLoop(0)

	// F: an HTTP conn on loop 0, and G's socket, ready to take F's number.
	fClient, fFD := adoptable543(t)
	gClient, gFD := adoptable543(t)
	var gClosed atomic.Bool
	t.Cleanup(func() {
		if !gClosed.Load() {
			_ = unix.Close(gFD)
		}
	})
	adoptOn885(t, l, fFD)

	q := newParker771(t, wl)
	var swapErr error
	var gOn sync.Once
	p := struct{ fd, peer int }{}
	p.fd, p.peer = socketpairNonblocking(t)
	pDone := make(chan struct{})
	if err := wl.RegisterConn(p.fd, func([]byte) {
		gOn.Do(func() {
			defer close(pDone)
			// On the loop thread: reap F, then accept G onto F's number.
			l.closeConn(fFD)
			if err := unix.Dup3(gFD, fFD, unix.O_CLOEXEC); err != nil {
				swapErr = err
				return
			}
			// Only the number F had refers to G's socket now: a second
			// descriptor on it would keep its epoll registration alive.
			gClosed.Store(true)
			_ = unix.Close(gFD)
			l.attachAdoptedFD(context.Background(), fFD, engine.Carryover{RemoteAddr: "G"}, time.Now().UnixNano())
		})
	}, func(error) {}); err != nil {
		t.Fatalf("RegisterConn(P): %v", err)
	}
	t.Cleanup(func() {
		_ = wl.UnregisterConn(p.fd)
		_ = unix.Close(p.fd)
		_ = unix.Close(p.peer)
	})
	// F's number is G's by now; the engine's stop closes it with the other
	// conns, or the stale event already did.

	if !workerRoundTrip(t, wl, 5*time.Second) {
		t.Fatal("round trip")
	}
	_, _ = unix.Write(q.peer, []byte{'q'})
	q.wait(t)
	_, _ = unix.Write(p.peer, []byte{'p'})
	_ = fClient.Close() // F's EPOLLIN|EPOLLRDHUP is ready behind P's
	close(q.release)
	select {
	case <-pDone:
	case <-time.After(5 * time.Second):
		t.Fatal("P's onRecv never ran")
	}
	if swapErr != nil {
		t.Fatalf("dup3 G onto F's number: %v", swapErr)
	}
	if !workerRoundTrip(t, wl, 5*time.Second) {
		t.Fatal("round trip after the swap")
	}
	if cs := slotOf885(l, fFD); cs == nil {
		t.Fatalf("the conn on F's number (G) is gone: F's stale event closed it")
	}

	// G must still be a live conn that is served.
	_ = gClient.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	var one [1]byte
	if n, err := gClient.Read(one[:]); n == 0 && err != nil {
		if ne, ok := err.(net.Error); !ok || !ne.Timeout() {
			t.Errorf("G's peer reads %v: F's stale event, dispatched on F's number after the swap, closed G", err)
		}
	}
	if _, err := gClient.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		t.Fatalf("write G's request: %v", err)
	}
	_ = gClient.SetReadDeadline(time.Now().Add(3 * time.Second))
	var resp [256]byte
	n, err := gClient.Read(resp[:])
	if n < 12 || string(resp[:12]) != "HTTP/1.1 200" {
		t.Errorf("G was not served: read %q, %v", resp[:n], err)
	}
}

// TestHTTPServedWhileADriverConnIsRegistered885 guards the other half of the
// tag: while a driver conn is registered, every HTTP event goes through the
// driver gate (driverCandidate, dispatchDriver), and an HTTP conn's tagged
// generation must not be taken for a driver registration that has ended, whose
// events dispatchDriver drops. The HTTP conn must be served, request after
// request, on the same loop.
func TestHTTPServedWhileADriverConnIsRegistered885(t *testing.T) {
	eng, stop := startHTTPEngine885(t)
	t.Cleanup(stop)
	l := eng.loops[0]
	wl := eng.WorkerLoop(0)

	dFD, dPeer := socketpairNonblocking(t)
	t.Cleanup(func() { _ = unix.Close(dFD); _ = unix.Close(dPeer) })
	if err := wl.RegisterConn(dFD, func([]byte) {}, func(error) {}); err != nil {
		t.Fatalf("RegisterConn(driver): %v", err)
	}
	t.Cleanup(func() { _ = wl.UnregisterConn(dFD) })
	if !l.hasDriverConns.Load() {
		t.Fatal("premise: hasDriverConns is not set with a driver conn registered")
	}

	client, fd := adoptable543(t)
	adoptOn885(t, l, fd)
	for i := range 3 {
		if _, err := client.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
			t.Fatalf("request %d: write: %v", i, err)
		}
		_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
		var resp [256]byte
		n, err := client.Read(resp[:])
		if n < 12 || string(resp[:12]) != "HTTP/1.1 200" {
			t.Fatalf("request %d on the HTTP conn, with a driver conn registered on its loop: read %q, %v", i, resp[:n], err)
		}
	}
}

// TestHTTPGenerationsAreTaggedAndWrap885 pins nextHTTPGen: bit 31 set, never
// 0, never a driver's, one per call, and a wrap of the 31-bit count that keeps
// the bit.
func TestHTTPGenerationsAreTaggedAndWrap885(t *testing.T) {
	l := newLedgerLoop(t)
	seen := map[uint32]bool{}
	for range 1000 {
		g := l.nextHTTPGen()
		if !httpTagged885(int32(g)) || g == 0 || isDriverGen(g) || seen[g] {
			t.Fatalf("nextHTTPGen returned %#x (tagged=%v driver=%v repeated=%v)", g, httpTagged885(int32(g)), isDriverGen(g), seen[g])
		}
		seen[g] = true
	}
	l.httpGen = maxDriverGen
	if g := l.nextHTTPGen(); g != httpGenBit {
		t.Errorf("the generation after the wrap is %#x, want %#x", g, httpGenBit)
	}
	if g := l.nextHTTPGen(); g != httpGenBit|1 {
		t.Errorf("the generation after that is %#x, want %#x", g, httpGenBit|1)
	}
}

// TestDispatchDriverLeavesHTTPGenerationsToTheHTTPPath885: an HTTP conn's
// generation is not a driver registration that has ended. With no driver conn
// registered the gate does not look it up; with one registered the event is
// looked up and, its number being nobody's, handed back to the HTTP path; on a
// driver conn's number it is stale and dropped.
func TestDispatchDriverLeavesHTTPGenerationsToTheHTTPPath885(t *testing.T) {
	eng, stop := newTestEngine(t)
	t.Cleanup(stop)
	wl := eng.WorkerLoop(0)
	l := eng.loops[0]
	tagged := httpGenBit | 7
	hangup := uint32(unix.EPOLLIN | unix.EPOLLRDHUP | unix.EPOLLHUP | unix.EPOLLERR)

	if l.hasDriverConns.Load() {
		t.Fatal("premise: a driver conn is registered")
	}
	if l.driverCandidate(tagged) {
		t.Error("an HTTP generation is looked up as a driver's with no driver conn registered")
	}

	x, xPeer := socketpairNonblocking(t)
	defer func() { _ = unix.Close(x); _ = unix.Close(xPeer) }()
	var closed atomic.Int32
	if err := wl.RegisterConn(x, func([]byte) {}, func(error) { closed.Add(1) }); err != nil {
		t.Fatalf("RegisterConn: %v", err)
	}
	defer func() { _ = wl.UnregisterConn(x) }()

	if !l.driverCandidate(tagged) {
		t.Error("an HTTP generation is not looked up while a driver conn is registered")
	}
	if l.dispatchDriver(x+1000, tagged, hangup) {
		t.Error("an HTTP generation on a number no driver conn has was taken for a driver's: its event would be dropped")
	}
	if !l.dispatchDriver(x, tagged, hangup) {
		t.Error("an HTTP generation on a driver conn's number was given to the HTTP path")
	}
	if n := closed.Load(); n != 0 {
		t.Errorf("a stale HTTP event closed the driver conn (%d onClose calls)", n)
	}
}
