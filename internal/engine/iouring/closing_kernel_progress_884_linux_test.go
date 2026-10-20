//go:build linux

package iouring

import (
	"net"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/engine/internal/errclass"
	"github.com/goceleris/celeris/internal/resource"
)

// celeris#884 item 52, the io_uring twin of celeris#876's review round 3. The
// closing drain's clock (cs.lastActivity, bound closingDrainBound: the longer
// of 5 s and WriteTimeout) moves once per SEND completion, and a SEND of more
// than the socket queue takes completes only as the queue drains. A client
// reading a Connection: close response through a deep send queue, slowly
// enough to go more than the bound between two completions, was reaped with the
// response truncated. The sweep now also samples the peer's acknowledged bytes
// (tcp_info bytes_acked) for a closing conn and counts any growth as progress,
// as epoll's noteClosingAcked does.
//
// The rig is a loopback TCP pair (a socketpair has no tcp_info, and then there
// is no evidence, which the tests in closing_drain_test.go cover): the conn the
// worker owns has a small send buffer and the peer a small receive buffer, so
// filling the former leaves a response tail queued in the kernel with no SEND
// completion to come, which is the shape of the defect. The drain clock is
// backdated instead of waited for, because the bound is 5 s at the least.

// newTCPClosingDrainWorker is newClosingDrainWorker on a loopback TCP pair. It
// returns the worker, its conn (fd: the server end, its send queue full) and
// the peer's end, a nonblocking descriptor the test reads from.
func newTCPClosingDrainWorker(t *testing.T, ring *Ring) (*Worker, *connState, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("loopback unavailable: %v", err)
	}
	defer func() { _ = ln.Close() }()
	d := net.Dialer{Timeout: 3 * time.Second, Control: func(_, _ string, c syscall.RawConn) error {
		return c.Control(func(fd uintptr) {
			_ = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_RCVBUF, 16<<10)
		})
	}}
	type dialed struct {
		c   net.Conn
		err error
	}
	dc := make(chan dialed, 1)
	go func() { c, err := d.Dial("tcp", ln.Addr().String()); dc <- dialed{c, err} }()
	sc, err := ln.Accept()
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	cl := <-dc
	if cl.err != nil {
		t.Fatalf("dial: %v", cl.err)
	}
	dup := func(c net.Conn) int {
		f, err := c.(*net.TCPConn).File()
		if err != nil {
			t.Fatalf("File: %v", err)
		}
		fd, err := unix.Dup(int(f.Fd()))
		_ = f.Close()
		_ = c.Close()
		if err != nil {
			t.Fatalf("dup: %v", err)
		}
		if err := unix.SetNonblock(fd, true); err != nil {
			t.Fatalf("nonblock: %v", err)
		}
		return fd
	}
	local, peer := dup(sc), dup(cl.c)
	t.Cleanup(func() { _ = unix.Close(peer) })
	_ = unix.SetsockoptInt(local, unix.SOL_SOCKET, unix.SO_SNDBUF, 64<<10)

	// Fill the kernel's send queue: the response tail the SEND in flight
	// would be waiting to place.
	chunk := make([]byte, 32<<10)
	for filled := 0; ; {
		n, err := unix.Write(local, chunk)
		if err == unix.EAGAIN {
			break
		}
		if err != nil {
			t.Fatalf("fill: %v", err)
		}
		if filled += n; filled > 64<<20 {
			t.Fatalf("the send queue never filled")
		}
	}
	time.Sleep(100 * time.Millisecond) // the peer's acks of what it could take settle

	w := &Worker{
		ring:        ring,
		conns:       make([]*connState, local+1),
		liveConns:   make([]int, 0, 4),
		errs:        &errclass.Counters{},
		activeConns: &atomic.Int64{},
		closeCount:  &atomic.Uint64{},
		cfg:         resource.Config{IdleTimeout: time.Hour},
	}
	w.cachedNow = time.Now().UnixNano()
	cs := &connState{
		fd:             local,
		liveIdx:        -1,
		generation:     7,
		sending:        true,
		kernelInflight: 1,
		sendBuf:        make([]byte, 1<<20), // the response tail the SEND has not placed
		lastActivity:   time.Now().UnixNano(),
	}
	w.conns[local] = cs
	w.addLiveConn(cs)
	w.connCount = 1
	w.activeConns.Add(1)
	return w, cs, peer
}

// peerReads takes up to n bytes off the peer's end and lets the acks settle.
func peerReads(t *testing.T, peer, n int) int {
	t.Helper()
	buf := make([]byte, 16<<10)
	got := 0
	for got < n {
		m, err := unix.Read(peer, buf)
		if err == unix.EAGAIN {
			break
		}
		if err != nil {
			t.Fatalf("peer read: %v", err)
		}
		if m == 0 {
			break
		}
		got += m
	}
	time.Sleep(100 * time.Millisecond)
	return got
}

// TestClosingDrainOfAReadingPeerIsNotCutAtTheBound884 is the defect: the peer
// takes bytes between two sweeps (here 256 KiB, which the SEND's completion has
// not shown), the drain's clock is past the bound, and the sweep must find the
// progress in the acknowledged bytes and keep the conn.
func TestClosingDrainOfAReadingPeerIsNotCutAtTheBound884(t *testing.T) {
	ring := newTestRing(t)
	w, cs, peer := newTCPClosingDrainWorker(t, ring)
	fd := cs.fd
	bound := time.Duration(w.closingDrainBound())
	cs.closing = true
	cs.lastActivity = time.Now().Add(-bound / 2).UnixNano() // inside the bound

	w.checkTimeouts() // the baseline sample
	if w.conns[fd] != cs {
		t.Fatalf("PREMISE: the sweep reaped a closing conn %v into a %v bound", bound/2, bound)
	}
	if got := peerReads(t, peer, 256<<10); got < 64<<10 {
		t.Fatalf("PREMISE: the peer took only %d bytes", got)
	}
	cs.closeSampled = 0 // the next sweep is due a sample
	cs.lastActivity = time.Now().Add(-bound - time.Second).UnixNano()
	w.checkTimeouts()
	if w.conns[fd] != cs {
		t.Fatalf("the closing conn was reaped %v after its drain clock last moved, though the peer had acknowledged bytes since "+
			"the last sample (the SEND had not completed): %v bound (celeris#884 item 52)", bound+time.Second, bound)
	}
	if now := time.Now().UnixNano(); now-cs.lastActivity > int64(time.Second) {
		t.Errorf("the sweep kept the conn but left the drain clock %v old", time.Duration(now-cs.lastActivity))
	}
}

// TestClosingDrainOfAStalledPeerIsStillReapedAtTheBound884 is the other half on
// the same rig: a peer that takes nothing between the samples gives no
// evidence, and the conn is reaped at the bound.
func TestClosingDrainOfAStalledPeerIsStillReapedAtTheBound884(t *testing.T) {
	ring := newTestRing(t)
	w, cs, _ := newTCPClosingDrainWorker(t, ring)
	fd := cs.fd
	bound := time.Duration(w.closingDrainBound())
	cs.closing = true
	cs.lastActivity = time.Now().Add(-bound / 2).UnixNano()

	w.checkTimeouts() // the baseline sample
	if w.conns[fd] != cs {
		t.Fatalf("PREMISE: the sweep reaped a closing conn %v into a %v bound", bound/2, bound)
	}
	time.Sleep(100 * time.Millisecond)
	cs.closeSampled = 0
	cs.lastActivity = time.Now().Add(-bound - time.Second).UnixNano()
	w.checkTimeouts()
	if w.conns[fd] != nil {
		t.Fatalf("a closing conn whose peer took nothing since the last sample was not reaped %v past its drain clock", bound+time.Second)
	}
}

// TestClosingDrainFirstMetAfterItsBoundIsGivenOneMoreBound884: a sweep that
// meets a closing conn long after its bound (a worker that was busy, a short
// WriteTimeout) has no earlier sample to compare with. The conn is not cut on
// no evidence: it is sampled, given one more bound, and reaped then if the peer
// takes nothing. epoll's noteClosingAcked does the same.
func TestClosingDrainFirstMetAfterItsBoundIsGivenOneMoreBound884(t *testing.T) {
	ring := newTestRing(t)
	w, cs, _ := newTCPClosingDrainWorker(t, ring)
	fd := cs.fd
	bound := time.Duration(w.closingDrainBound())
	cs.closing = true
	cs.lastActivity = time.Now().Add(-10 * bound).UnixNano() // the close was asked long ago

	w.checkTimeouts()
	if w.conns[fd] != cs {
		t.Fatalf("the first sweep to meet a closing conn %v past its %v bound reaped it on no evidence", 10*bound, bound)
	}
	time.Sleep(100 * time.Millisecond)
	cs.closeSampled = 0
	cs.lastActivity = time.Now().Add(-bound - time.Second).UnixNano()
	w.checkTimeouts()
	if w.conns[fd] != nil {
		t.Fatalf("a closing conn whose peer reads nothing was not reaped at its second bound")
	}
}

// TestClosingDrainOfAFixedFileConnIsNotSampled884: a fixed-file conn's fd is a
// slot in the ring's table, not a descriptor, so getsockopt on it would read
// another socket's counters (or none). Its drain keeps the SEND-completion
// clock alone.
func TestClosingDrainOfAFixedFileConnIsNotSampled884(t *testing.T) {
	ring := newTestRing(t)
	w, cs, peer := newTCPClosingDrainWorker(t, ring)
	fd := cs.fd
	bound := time.Duration(w.closingDrainBound())
	cs.closing = true
	cs.fixedFile = true
	cs.lastActivity = time.Now().Add(-bound / 2).UnixNano()
	w.checkTimeouts()
	if got := peerReads(t, peer, 256<<10); got < 64<<10 {
		t.Fatalf("PREMISE: the peer took only %d bytes", got)
	}
	cs.closeSampled = 0
	cs.lastActivity = time.Now().Add(-bound - time.Second).UnixNano()
	w.checkTimeouts()
	if cs.closeAckedKnown || cs.closeSampled != 0 {
		t.Errorf("a fixed-file conn was sampled (known=%v sampledAt=%d)", cs.closeAckedKnown, cs.closeSampled)
	}
	if w.conns[fd] != nil {
		t.Fatalf("a fixed-file closing conn past its drain clock was kept on evidence read from its slot number")
	}
}

// TestClosingDrainIsSampledBeforeItIsReapedWhateverTheSampleRate884: samples are
// rate limited to a quarter of the bound and a conn's first sample is only a
// baseline. A worker whose sweeps are irregular (ReadHeaderTimeout off with
// mixed load) can meet a closing conn late in its bound (the baseline, 0.9
// bound in) and sweep again past the bound but inside a quarter of it from the
// baseline: the bound test used to reap the conn on the baseline's evidence
// though the peer had acknowledged bytes since. The sweep that is about to reap
// takes a fresh sample first. The stamps are moved back by hand for the time
// between the sweeps, as the tests above do.
func TestClosingDrainIsSampledBeforeItIsReapedWhateverTheSampleRate884(t *testing.T) {
	ring := newTestRing(t)
	w, cs, peer := newTCPClosingDrainWorker(t, ring)
	fd := cs.fd
	bound := time.Duration(w.closingDrainBound())
	cs.closing = true
	cs.lastActivity = time.Now().Add(-bound * 9 / 10).UnixNano()

	w.checkTimeouts() // the baseline, 0.9 bound into the drain
	if w.conns[fd] != cs || cs.closeSampled == 0 || !cs.closeAckedKnown {
		t.Fatalf("PREMISE: the first sweep did not keep the conn and take its baseline")
	}
	if got := peerReads(t, peer, 256<<10); got < 64<<10 {
		t.Fatalf("PREMISE: the peer took only %d bytes", got)
	}
	shift := int64(bound * 15 / 100) // past the bound, inside bound/4 of the baseline
	cs.lastActivity -= shift
	cs.closeSampled -= shift
	w.checkTimeouts()
	if w.conns[fd] != cs {
		t.Fatalf("a closing conn whose peer acknowledged bytes since the baseline sample was reaped at 1.05 bound without a fresh sample (the rate limit of bound/4 stood in for it)")
	}
	if now := time.Now().UnixNano(); now-cs.lastActivity > int64(time.Second) {
		t.Errorf("the sweep kept the conn but left the drain clock %v old", time.Duration(now-cs.lastActivity))
	}
}
