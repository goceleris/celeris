//go:build linux

package iouring

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/conn"
	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/engine/internal/errclass"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
)

// celeris#884 item 73: the unit half of the shutdown gate's look at a conn
// (shutdown_gate_884.go). The wire-level half, with the engine and clients, is
// test/server TestIOUringShutdownGateConns884.

// gateNoHandler884 fails the test if a request reaches it: the parser is fed
// partial requests here, which it buffers.
type gateNoHandler884 struct{ t *testing.T }

func (h gateNoHandler884) HandleStream(_ context.Context, _ *stream.Stream) error {
	h.t.Error("a handler ran for a partial request")
	return nil
}

// gateConn884 returns an HTTP/1 conn (decided, state fresh) that is idle.
func gateConn884(fd int) *connState {
	cs := &connState{fd: fd, liveIdx: -1, detected: true, h1State: conn.NewH1State()}
	cs.protocol.Store(int32(engine.HTTP1))
	return cs
}

// feed884 hands the parser part of a request: it keeps it, in progress.
func feed884(t *testing.T, cs *connState, part string) {
	t.Helper()
	write := func([]byte) {}
	if err := conn.ProcessH1(context.Background(), []byte(part), cs.h1State, gateNoHandler884{t}, write); err != nil {
		t.Fatalf("ProcessH1(%q): %v", part, err)
	}
	if !cs.h1State.HasPendingData() {
		t.Fatalf("setup: %q left no pending state", part)
	}
}

// TestGateLookAtAConn884 is the table of what the gate does with a conn: whether
// handleRecv reads its bytes on (gateReadsOn), whether the pass may close it
// (gateCloseable), and whether it is marked close-after. A conn the gate reads
// on must not be closeable, and the other way round for an idle one.
func TestGateLookAtAConn884(t *testing.T) {
	w := &Worker{}
	for _, tc := range []struct {
		name       string
		build      func(t *testing.T) *connState
		readsOn    bool
		closeable  bool
		closeAfter bool // set on the conn by the look at it
	}{
		{"idle-keepalive", func(t *testing.T) *connState { return gateConn884(7) }, false, true, true},
		{"no-byte-undetected", func(t *testing.T) *connState { return &connState{fd: 7, liveIdx: -1} }, false, true, false},
		{"undecided-with-bytes", func(t *testing.T) *connState {
			return &connState{fd: 7, liveIdx: -1, detectAccum: []byte("PRI ")}
		}, true, false, false},
		{"headers-in-pieces", func(t *testing.T) *connState {
			cs := gateConn884(7)
			feed884(t, cs, "GET / HTTP/1.1\r\nHost: x\r\n")
			return cs
		}, true, false, true},
		{"body-arriving", func(t *testing.T) *connState {
			cs := gateConn884(7)
			feed884(t, cs, "POST / HTTP/1.1\r\nHost: x\r\nContent-Length: 100\r\n\r\nabc")
			return cs
		}, true, false, true},
		{"body-into-bodybuf", func(t *testing.T) *connState {
			cs := gateConn884(7)
			cs.recvIntoBody = true
			return cs
		}, true, false, true},
		{"response-queued", func(t *testing.T) *connState {
			cs := gateConn884(7)
			cs.writeBuf = []byte("HTTP/1.1 200 OK\r\n")
			return cs
		}, false, false, true},
		{"send-in-flight", func(t *testing.T) *connState {
			cs := gateConn884(7)
			cs.sending = true
			return cs
		}, false, false, true},
		{"zc-notification-owed", func(t *testing.T) *connState {
			cs := gateConn884(7)
			cs.zcNotifPending = true
			return cs
		}, false, false, true},
		{"detached-stream-running", func(t *testing.T) *connState {
			cs := gateConn884(7)
			cs.detachMu = &sync.Mutex{}
			cs.h1State.Detached.Store(true)
			return cs
		}, false, false, false},
		{"detached-stream-ended", func(t *testing.T) *connState {
			cs := gateConn884(7)
			cs.detachMu = &sync.Mutex{}
			cs.h1State.Detached.Store(true)
			cs.detachClosed = true
			return cs
		}, false, true, false},
		{"detached-stream-ended-queue-left", func(t *testing.T) *connState {
			cs := gateConn884(7)
			cs.detachMu = &sync.Mutex{}
			cs.h1State.Detached.Store(true)
			cs.detachClosed = true
			cs.writeBuf = []byte("tick")
			return cs
		}, false, false, false},
		{"promoted-idle-parked", func(t *testing.T) *connState {
			cs := gateConn884(7)
			cs.detachMu = &sync.Mutex{}
			cs.asyncPromoted.Store(true)
			cs.asyncRun, cs.asyncParked = true, true
			return cs
		}, false, true, true},
		{"promoted-goroutine-gone", func(t *testing.T) *connState {
			cs := gateConn884(7)
			cs.detachMu = &sync.Mutex{}
			cs.asyncPromoted.Store(true)
			return cs
		}, false, true, true},
		{"promoted-input-waiting", func(t *testing.T) *connState {
			cs := gateConn884(7)
			cs.detachMu = &sync.Mutex{}
			cs.asyncPromoted.Store(true)
			cs.asyncRun, cs.asyncParked = true, true
			cs.asyncInBuf = []byte("GET / HTTP/1.1\r\n")
			return cs
		}, true, false, false},
		{"promoted-inside-a-handler", func(t *testing.T) *connState {
			cs := gateConn884(7)
			cs.detachMu = &sync.Mutex{}
			cs.detachMu.Lock() // the goroutine holds it across the handler
			cs.asyncPromoted.Store(true)
			cs.asyncRun = true
			return cs
		}, true, false, false},
		{"promoted-partial-request", func(t *testing.T) *connState {
			cs := gateConn884(7)
			cs.detachMu = &sync.Mutex{}
			cs.asyncPromoted.Store(true)
			cs.asyncRun, cs.asyncParked = true, true
			feed884(t, cs, "POST / HTTP/1.1\r\nHost: x\r\nContent-Length: 100\r\n\r\nabc")
			return cs
		}, true, false, true},
		{"promoted-lock-busy-parked", func(t *testing.T) *connState {
			cs := gateConn884(7)
			cs.detachMu = &sync.Mutex{}
			cs.detachMu.Lock() // a guarded write holds it
			cs.asyncPromoted.Store(true)
			cs.asyncRun, cs.asyncParked = true, true
			return cs
		}, true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs := tc.build(t)
			if got := w.gateReadsOn(cs); got != tc.readsOn {
				t.Errorf("gateReadsOn=%v, want %v", got, tc.readsOn)
			}
			if cs.h1State != nil {
				cs.h1State.CloseAfter = false
			}
			if got := w.gateCloseable(cs); got != tc.closeable {
				t.Errorf("gateCloseable=%v, want %v", got, tc.closeable)
			}
			if h := cs.h1State; h != nil && h.CloseAfter != tc.closeAfter {
				t.Errorf("after the pass's look the conn's CloseAfter is %v, want %v: a request answered after the gate must carry Connection: close", h.CloseAfter, tc.closeAfter)
			}
			if tc.readsOn && tc.closeable {
				t.Error("the table has a conn the gate both reads on and may close")
			}
		})
	}
}

// gateRig884 is a worker with a ring and n conns on real descriptors (one end
// of a socketpair each), each idle HTTP/1, as closeConn needs to close them.
func gateRig884(t *testing.T, n int) (*Worker, []*connState, []int) {
	t.Helper()
	ring := newTestRing(t)
	w := &Worker{
		ring:        ring,
		liveConns:   make([]int, 0, n),
		errs:        &errclass.Counters{},
		activeConns: &atomic.Int64{},
		closeCount:  &atomic.Uint64{},
		recvArm:     &recvArmStats{},
		handoffLoss: &handoffLossStats{},
		listenFD:    -1,
	}
	w.cachedNow = time.Now().UnixNano()
	var css []*connState
	var peers []int
	for range n {
		fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
		if err != nil {
			t.Fatalf("socketpair: %v", err)
		}
		local, peer := fds[0], fds[1]
		t.Cleanup(func() { _ = unix.Close(peer) })
		cs := gateConn884(local)
		cs.generation = 9
		cs.buf = make([]byte, 4096)
		css = append(css, cs)
		peers = append(peers, peer)
	}
	w.conns = make([]*connState, 0)
	maxfd := 0
	for _, cs := range css {
		maxfd = max(maxfd, cs.fd)
	}
	w.conns = make([]*connState, maxfd+1)
	for _, cs := range css {
		w.conns[cs.fd] = cs
		w.addLiveConn(cs)
		w.connCount++
		w.activeConns.Add(1)
	}
	t.Cleanup(func() {
		// What the test left open: closeConn's close is deferred behind the
		// recv-cancel's completion, which no loop runs here.
		for _, cs := range css {
			_ = unix.Close(cs.fd)
		}
	})
	return w, css, peers
}

func liveGate884(w *Worker, css []*connState) (live int) {
	for _, cs := range css {
		if w.conns[cs.fd] == cs {
			live++
		}
	}
	return live
}

// TestGatePassClosesAtMostTheCapPerPass884: the pass closes gateClosesPerPass
// idle conns and no more, and the next one goes on, so that a worker with
// thousands of idle conns is not stopped for the length of all their closes.
func TestGatePassClosesAtMostTheCapPerPass884(t *testing.T) {
	n := gateClosesPerPass + 6
	w, css, _ := gateRig884(t, n)
	w.closeIdleH1()
	if got, want := liveGate884(w, css), n-gateClosesPerPass; got != want {
		t.Fatalf("after one pass %d of %d conns are open, want %d: the pass must close exactly the cap (%d)", got, n, want, gateClosesPerPass)
	}
	w.closeIdleH1()
	if got := liveGate884(w, css); got != 0 {
		t.Fatalf("after the second pass %d conns are still open, want 0", got)
	}
}

// TestGatePassSkipsHTTP2AndBusyConns884: only HTTP/1 conns are the pass's (an
// HTTP/2 conn has had GOAWAY and is served on), and one with a response going
// out is left for the next pass.
func TestGatePassSkipsHTTP2AndBusyConns884(t *testing.T) {
	w, css, _ := gateRig884(t, 4)
	css[0].protocol.Store(int32(engine.H2C))
	css[1].writeBuf = []byte("HTTP/1.1 200 OK\r\n\r\n")
	w.closeIdleH1()
	for i, want := range []bool{true, true, false, false} {
		if got := w.conns[css[i].fd] == css[i]; got != want {
			t.Errorf("conn %d open=%v after the pass, want %v", i, got, want)
		}
	}
}

// TestGatePassWaitsForTheKernelSendQueue884: a conn whose response has left
// the engine's queues but not the kernel's (the peer has not taken it) is not
// idle: it is closed once the peer has, not before.
func TestGatePassWaitsForTheKernelSendQueue884(t *testing.T) {
	w, css, peers := gateRig884(t, 1)
	// A TCP pair in place of the socketpair, to have a send queue the kernel
	// holds: SIOCOUTQ is the unacknowledged bytes.
	lfd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(lfd) }()
	if err := unix.Bind(lfd, &unix.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatal(err)
	}
	if err := unix.Listen(lfd, 1); err != nil {
		t.Fatal(err)
	}
	sa, _ := unix.Getsockname(lfd)
	peer, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(peer) }()
	if err := unix.SetsockoptInt(peer, unix.SOL_SOCKET, unix.SO_RCVBUF, 4096); err != nil {
		t.Fatal(err)
	}
	if err := unix.Connect(peer, sa); err != nil {
		t.Fatal(err)
	}
	local, _, err := unix.Accept4(lfd, unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC)
	if err != nil {
		t.Fatal(err)
	}
	// Move the conn onto the TCP socket.
	old := css[0]
	w.removeLiveConn(old)
	w.clearConnSlot(old.fd)
	w.connCount--
	_ = unix.Close(old.fd)
	_ = peers
	cs := gateConn884(local)
	cs.generation = 9
	cs.buf = make([]byte, 4096)
	if local >= len(w.conns) {
		w.conns = append(w.conns, make([]*connState, local+1-len(w.conns))...)
	}
	w.conns[local] = cs
	w.addLiveConn(cs)
	w.connCount++
	// Fill the kernel's send queue: the peer reads nothing.
	chunk := make([]byte, 64<<10)
	for {
		if _, err := unix.Write(local, chunk); err != nil {
			break
		}
	}
	if q, _ := unix.IoctlGetInt(local, unix.TIOCOUTQ); q == 0 {
		t.Fatal("setup: the kernel's send queue is empty")
	}

	w.closeIdleH1()
	if w.conns[local] != cs {
		t.Fatal("the pass closed a conn whose response is still in the kernel's send queue, unread by its peer: a close with unread bytes from the peer would reset it away")
	}
	// The peer reads it all; the queue empties, and the next pass closes the conn.
	buf := make([]byte, 64<<10)
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); {
		_, _ = unix.Read(peer, buf)
		if q, _ := unix.IoctlGetInt(local, unix.TIOCOUTQ); q == 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if q, _ := unix.IoctlGetInt(local, unix.TIOCOUTQ); q != 0 {
		t.Fatalf("setup: the kernel's send queue still holds %d bytes after the peer read", q)
	}
	w.closeIdleH1()
	if w.conns[local] == cs {
		t.Error("the pass left an idle conn with an empty send queue open")
	}
	_ = unix.Close(local)
}
