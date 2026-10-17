//go:build linux

package epoll

import (
	"bytes"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/conn"
	"github.com/goceleris/celeris/internal/engine"
)

// celeris#885 item 42, unit level: a truly-detached conn with a remainder
// the socket would not take is handed to EPOLLOUT and leaves the dirty list
// (the dirty list holds the loop at a 0 ms epoll_wait, a spin), and every
// EPOLL_CTL_MOD of a conn derives its interest from the conn's state, so
// arming EPOLLOUT cannot re-enable the EPOLLIN a paused conn gave up (and a
// pause cannot drop the EPOLLOUT an armed conn waits for). The end-to-end
// CPU window is dirty_busy_poll_885_linux_test.go.

// detachedPair885 is a truly-detached conn on a bare loop, registered in the
// loop's epoll set with the interest an HTTP conn is added with. peer is the
// other end of the socketpair.
func detachedPair885(t *testing.T) (l *Loop, cs *connState, peer int) {
	t.Helper()
	l = newReapLoop(t)
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Skipf("socketpair unavailable: %v", err)
	}
	local := pair[0]
	peer = pair[1]
	t.Cleanup(func() { _ = unix.Close(peer); _ = unix.Close(local) })
	if local >= connTableSize {
		t.Skip("fd out of table range")
	}
	for _, fd := range pair {
		if err := unix.SetNonblock(fd, true); err != nil {
			t.Fatalf("set nonblock: %v", err)
		}
	}
	cs = &connState{fd: local, liveIdx: -1}
	cs.h1State = conn.NewH1State()
	cs.h1State.Detached.Store(true)
	cs.detachMu = &sync.Mutex{}
	ev := cs.epollEvent(unix.EPOLLIN | unix.EPOLLET | unix.EPOLLRDHUP)
	if err := unix.EpollCtl(l.epollFD, unix.EPOLL_CTL_ADD, local, &ev); err != nil {
		t.Fatalf("epoll_ctl ADD: %v", err)
	}
	l.conns[local] = cs
	l.addLiveConn(cs)
	l.connCount++
	l.activeConns.Add(1)
	l.detachedCount = 1
	return l, cs, peer
}

// TestFlushDirtyHandsADetachedConnToEPOLLOUT885: the dirty pass finds a
// detached conn's remainder still partial. The conn leaves the list and waits
// on EPOLLOUT; the loop then does not poll (adaptiveTimeoutMs is not 0), and
// the EPOLLOUT edge the peer's reading raises is handled by handleWritable,
// which flushes the rest, in order, and disarms.
func TestFlushDirtyHandsADetachedConnToEPOLLOUT885(t *testing.T) {
	l, cs, peer := detachedPair885(t)
	filler := fillSendBuffer(t, cs.fd)
	want := make([]byte, 192<<10)
	for i := range want {
		want[i] = byte(i*7 + 3)
	}
	cs.writeBuf = append(cs.writeBuf, want...)
	cs.pendingBytes = len(want)
	l.markDirty(cs)

	l.flushDirty()

	if cs.dirty || l.dirtyHead != nil {
		t.Fatalf("celeris#885 item 42: the detached conn is still on the dirty list after a partial flush: " +
			"adaptiveTimeoutMs returns 0 for it, a spin")
	}
	if !cs.epollOut {
		t.Fatalf("the detached conn waits for neither the dirty list nor EPOLLOUT: nothing would flush it")
	}
	if got := l.adaptiveTimeoutMs(500); got == 0 {
		t.Errorf("adaptiveTimeoutMs = 0 with the only conn parked on EPOLLOUT")
	}

	// The peer reads; every EPOLLOUT edge the loop would be woken by is handed
	// to handleWritable, as the run loop does.
	var got bytes.Buffer
	events := make([]unix.EpollEvent, 8)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && cs.epollOut {
		drainPeer(t, peer, &got)
		n, err := unix.EpollWait(l.epollFD, events, 50)
		if err != nil && err != unix.EINTR {
			t.Fatalf("epoll_wait: %v", err)
		}
		for _, ev := range events[:max(n, 0)] {
			if ev.Events&unix.EPOLLOUT != 0 && int(ev.Fd) == cs.fd && cs.epollOut {
				l.handleWritable(cs)
			}
		}
	}
	drainPeer(t, peer, &got)
	if cs.epollOut || csWritePending(cs) {
		t.Fatalf("the remainder was not flushed by the EPOLLOUT edge: epollOut=%v pending=%d",
			cs.epollOut, csPendingBytes(cs))
	}
	if g := got.Bytes(); len(g) != filler+len(want) || !bytes.Equal(g[filler:], want) {
		t.Fatalf("the peer got %d bytes, want %d with the staged remainder last and in order", len(g), filler+len(want))
	}
}

// interest885 is the interest mask the kernel holds for fd in epfd, from
// /proc/self/fdinfo/<epfd> ("tfd: N events: HEX data: HEX"), without
// EPOLLERR and EPOLLHUP, which the kernel always adds.
func interest885(t *testing.T, epfd, fd int) uint32 {
	t.Helper()
	raw, err := os.ReadFile("/proc/self/fdinfo/" + strconv.Itoa(epfd))
	if err != nil {
		t.Skipf("fdinfo unavailable: %v", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		f := strings.Fields(line)
		if len(f) >= 4 && f[0] == "tfd:" && f[1] == strconv.Itoa(fd) && f[2] == "events:" {
			v, err := strconv.ParseUint(f[3], 16, 32)
			if err != nil {
				t.Fatalf("fdinfo line %q: %v", line, err)
			}
			return uint32(v) &^ (unix.EPOLLERR | unix.EPOLLHUP)
		}
	}
	t.Fatalf("fd %d is not in the epoll set (fdinfo: %q)", fd, raw)
	return 0
}

// pauseOrResume885 applies the middleware's request through the production
// path, drainDetachQueue (the other, drainRead's inline one, shares its MOD).
func pauseOrResume885(l *Loop, cs *connState, pause bool) {
	cs.recvPauseDesired.Store(pause)
	l.detachQMu.Lock()
	l.detachQueue = append(l.detachQueue, cs)
	l.detachQPending.Store(1)
	l.detachQMu.Unlock()
	l.drainDetachQueue()
	l.removeDirty(cs) // the hand-back puts the conn on the list
}

// TestEveryMODDerivesItsInterestFromTheConnsState885: the interest the kernel
// holds after each transition is IN unless recv is paused, OUT while armed,
// RDHUP and ET always. Reads the mask from fdinfo, so nothing depends on
// timing.
func TestEveryMODDerivesItsInterestFromTheConnsState885(t *testing.T) {
	const (
		in    = unix.EPOLLIN
		out   = unix.EPOLLOUT
		rdhup = unix.EPOLLRDHUP
		et    = unix.EPOLLET
	)
	for _, tc := range []struct {
		name string
		do   func(l *Loop, cs *connState)
		want uint32
	}{
		{"arm", func(l *Loop, cs *connState) { l.armEpollOut(cs) }, in | out | rdhup | et},
		{"arm-then-disarm", func(l *Loop, cs *connState) { l.armEpollOut(cs); l.disarmEpollOut(cs) }, in | rdhup | et},
		{"pause", func(l *Loop, cs *connState) { pauseOrResume885(l, cs, true) }, rdhup | et},
		{"pause-then-arm", func(l *Loop, cs *connState) {
			pauseOrResume885(l, cs, true)
			l.armEpollOut(cs)
		}, out | rdhup | et},
		{"pause-arm-disarm", func(l *Loop, cs *connState) {
			pauseOrResume885(l, cs, true)
			l.armEpollOut(cs)
			l.disarmEpollOut(cs)
		}, rdhup | et},
		{"arm-then-pause", func(l *Loop, cs *connState) {
			l.armEpollOut(cs)
			pauseOrResume885(l, cs, true)
		}, out | rdhup | et},
		{"pause-arm-resume", func(l *Loop, cs *connState) {
			pauseOrResume885(l, cs, true)
			l.armEpollOut(cs)
			pauseOrResume885(l, cs, false)
		}, in | out | rdhup | et},
		{"pause-resume", func(l *Loop, cs *connState) {
			pauseOrResume885(l, cs, true)
			pauseOrResume885(l, cs, false)
		}, in | rdhup | et},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, cs, _ := detachedPair885(t)
			tc.do(l, cs)
			if got := interest885(t, l.epollFD, cs.fd); got != tc.want {
				t.Errorf("interest after %s = %#x, want %#x (IN %#x, OUT %#x, RDHUP %#x, ET %#x)",
					tc.name, got, tc.want, uint32(in), uint32(out), uint32(rdhup), uint32(et))
			}
		})
	}
}

// TestDrainReadsInlineFlushHandsADetachedConnToEPOLLOUT885: the other site. A
// frame arrives on a detached conn whose queue the socket will not take (the
// middleware goroutine's writes have filled it), drainRead's inline flush
// finds it partial, and it used to put the conn on the dirty list. It arms
// EPOLLOUT, as for any other conn.
func TestDrainReadsInlineFlushHandsADetachedConnToEPOLLOUT885(t *testing.T) {
	l, cs, peer := detachedPair885(t)
	cs.buf = make([]byte, 4096)
	cs.protocol = engine.HTTP1
	cs.detected = true
	want := make([]byte, 64<<10)
	for i := range want {
		want[i] = byte(i*13 + 1)
	}
	// What the middleware's write hook leaves in the queue while the frame is
	// being delivered: the write the goroutine could not make yet.
	cs.h1State.WSDataDelivery = func([]byte) {
		cs.writeBuf = append(cs.writeBuf, want...)
		cs.pendingBytes += len(want)
	}
	fillSendBuffer(t, cs.fd)
	if n, err := unix.Write(peer, []byte("frame")); n != 5 || err != nil {
		t.Fatalf("peer write: %d, %v", n, err)
	}

	l.drainRead(cs.fd, time.Now().UnixNano())

	if !csWritePending(cs) {
		t.Fatal("premise: the socket took the whole queue")
	}
	if cs.dirty || l.dirtyHead != nil {
		t.Fatalf("celeris#885 item 42: drainRead's inline flush left the detached conn on the dirty list")
	}
	if !cs.epollOut {
		t.Fatalf("the detached conn waits for neither the dirty list nor EPOLLOUT")
	}
}

// TestFlushCarriedReplayHandsADetachedConnToEPOLLOUT885: the third site. A
// replay's flush finds the queue of a detached conn partial (a conn adopted
// from a hand-off whose middleware was already detached) and used to keep it
// on the dirty list, where the first dirty pass handed it to EPOLLOUT anyway
// at the cost of one wasted write that returned EAGAIN. It arms EPOLLOUT, as
// for any other conn, and the list stays empty.
func TestFlushCarriedReplayHandsADetachedConnToEPOLLOUT885(t *testing.T) {
	l, cs, _ := detachedPair885(t)
	fillSendBuffer(t, cs.fd)
	cs.writeBuf = append(cs.writeBuf, make([]byte, 64<<10)...)
	cs.pendingBytes = len(cs.writeBuf)

	l.flushCarriedReplay(cs, cs.fd)

	if !csWritePending(cs) {
		t.Fatal("premise: the socket took the whole queue")
	}
	if cs.dirty || l.dirtyHead != nil {
		t.Fatalf("celeris#885 item 42: flushCarriedReplay left the detached conn on the dirty list")
	}
	if !cs.epollOut {
		t.Fatalf("the detached conn waits for neither the dirty list nor EPOLLOUT")
	}
}
