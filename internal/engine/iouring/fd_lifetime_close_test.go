//go:build linux

package iouring

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/conn"
	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/engine/internal/errclass"
	"github.com/goceleris/celeris/internal/resource"
)

// The fd-lifetime rule on the close paths (celeris#685): a descriptor number
// is released only when no op that names it can still be issued. These run in
// every build and need one ring (or one worker), so they run in the CI `unit`
// job's shape; the trials that force the theft itself are the -tags=validation
// TestRecvTheft715ArmA, TestRecvTheft685Linked and TestRecvTheft685Hijack*.

// TestPendingReleaseEntryStaysTwentyFourBytes pins that the celeris#685 hold
// (holdsFD, fd) fits in the padding after detached: the queue is appended to
// on every close.
func TestPendingReleaseEntryStaysTwentyFourBytes(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		t.Skip("64-bit layout only")
	}
	if n := unsafe.Sizeof(pendingReleaseEntry{}); n != 24 {
		t.Fatalf("pendingReleaseEntry is %d bytes, want 24", n)
	}
}

// newOwedCloseWorker returns a synthetic worker with a real ring and one H1
// connection on a socketpair, and the peer end (never read by the worker).
// detached gives the connection a detachMu, which routes closeConn to
// finishCloseDetached, as for an async-dispatch connection.
func newOwedCloseWorker(t *testing.T, detached bool) (*Worker, *connState, int) {
	t.Helper()
	ring := newTestRing(t)
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_NONBLOCK, 0)
	if err != nil {
		t.Skipf("socketpair: %v", err)
	}
	local, peer := pair[0], pair[1]
	localTarget := fdTarget(local)
	t.Cleanup(func() {
		_ = unix.Close(peer)
		// Only if the test left it open: the number may be someone else's
		// once the release closed it.
		if fdTarget(local) == localTarget {
			_ = unix.Close(local)
		}
	})
	w := &Worker{
		ring:        ring,
		conns:       make([]*connState, local+1),
		liveConns:   make([]int, 0, 4),
		errs:        &errclass.Counters{},
		activeConns: &atomic.Int64{},
		closeCount:  &atomic.Uint64{},
		recvArm:     &recvArmStats{},
		handoffLoss: &handoffLossStats{},
	}
	w.cachedNow = time.Now().UnixNano()
	cs := &connState{
		fd:         local,
		liveIdx:    -1,
		generation: 11,
		buf:        make([]byte, 4096),
		h1State:    conn.NewH1State(),
		detected:   true,
	}
	cs.protocol.Store(int32(engine.HTTP1))
	if detached {
		cs.detachMu = new(sync.Mutex)
	}
	w.conns[local] = cs
	w.addLiveConn(cs)
	w.connCount = 1
	w.activeConns.Add(1)
	return w, cs, peer
}

// runRingOnce submits what the ring holds, waits up to d for completions, and
// feeds every recv/send completion to staleConnCQE, as the loop does.
// Returns the recv completions' results.
func runRingOnce(t *testing.T, w *Worker, d time.Duration) []int32 {
	t.Helper()
	if err := w.ring.SubmitAndWaitTimeout(d); err != nil {
		t.Fatalf("submit: %v", err)
	}
	var res []int32
	head, tail := w.ring.BeginCQ()
	for ; head != tail; head++ {
		c := w.ring.cqeAt(head)
		ud := c.UserData
		switch ud & udMask {
		case udRecv, udSend:
			if ud&udMask == udRecv {
				res = append(res, c.Res)
			}
			w.staleConnCQE(c, int(ud&fdMask), ud)
		}
	}
	w.ring.EndCQ(head)
	return res
}

// peerSawEnd reports whether the peer reads EOF (or a reset) within d.
func peerSawEnd(peer int, d time.Duration) bool {
	buf := make([]byte, 64)
	for end := time.Now().Add(d); time.Now().Before(end); {
		n, err := unix.Read(peer, buf)
		if n == 0 && err == nil {
			return true
		}
		if err != nil && !errors.Is(err, unix.EAGAIN) && !errors.Is(err, unix.EINTR) {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return false
}

// TestFinishCloseKeepsDescriptorWhileRecvOwed is the rule's mechanism on the
// H1 fast path: a close with a recv SQE placed and not submitted (the #715
// precondition) must leave the descriptor open, shut its read side, and hand
// it to its pendingRelease entry; the recv, issued by the next enter, ends at
// once against this socket; the release then closes the descriptor.
func TestFinishCloseKeepsDescriptorWhileRecvOwed(t *testing.T) {
	for _, detached := range []bool{false, true} {
		t.Run(map[bool]string{false: "fast", true: "detached"}[detached], func(t *testing.T) {
			w, cs, peer := newOwedCloseWorker(t, detached)
			fd := cs.fd
			target := fdTarget(fd)
			if !w.prepareRecv(cs, cs.buf) || cs.kernelInflight != 1 {
				t.Fatalf("recv not placed: kernelInflight=%d", cs.kernelInflight)
			}
			w.closeConn(fd)
			if w.conns[fd] != nil {
				t.Fatal("the conn is still registered after closeConn")
			}
			if got := fdTarget(fd); got != target {
				t.Fatalf("fd %d was released with its recv still unsubmitted (now %q, was %q)", fd, got, target)
			}
			if len(w.pendingRelease) != 1 || !w.pendingRelease[0].holdsFD || int(w.pendingRelease[0].fd) != fd || w.closeFDOwed != 1 {
				t.Fatalf("the kept descriptor is not on its pendingRelease entry: %+v closeFDOwed=%d", w.pendingRelease, w.closeFDOwed)
			}
			// The async path shuts the write side down too, as it always did:
			// its client sees the FIN now, not at the release.
			if detached && !peerSawEnd(peer, time.Second) {
				t.Error("finishCloseDetached kept the descriptor but its peer saw no FIN")
			}
			w.drainPendingRelease()
			if fdTarget(fd) != target {
				t.Fatal("drainPendingRelease closed the descriptor before the owed recv ended")
			}
			var res []int32
			for end := time.Now().Add(2 * time.Second); cs.kernelInflight > 0 && time.Now().Before(end); {
				res = append(res, runRingOnce(t, w, 50*time.Millisecond)...)
			}
			if cs.kernelInflight != 0 {
				t.Fatalf("the owed recv never ended: kernelInflight=%d", cs.kernelInflight)
			}
			w.cachedNow = time.Now().UnixNano()
			w.drainPendingRelease()
			if fdTarget(fd) == target {
				t.Fatalf("fd %d still open after its owed recv ended (recv results %v)", fd, res)
			}
			if w.closeFDOwed != 0 || len(w.pendingRelease) != 0 {
				t.Fatalf("closeFDOwed=%d pendingRelease=%d after the release, want 0 and 0", w.closeFDOwed, len(w.pendingRelease))
			}
			if !peerSawEnd(peer, time.Second) {
				t.Error("the peer saw no end of the connection after the release")
			}
			t.Logf("celeris685 mechanism detached=%v recv_results=%v", detached, res)
		})
	}
}

// TestFinishCloseClosesAtOnceWhenNothingOwed is the other half: with no op
// owed (the sync Connection: close response, a client's FIN) the close is
// what it was, the descriptor closed at once and nothing held.
func TestFinishCloseClosesAtOnceWhenNothingOwed(t *testing.T) {
	w, cs, peer := newOwedCloseWorker(t, false)
	fd := cs.fd
	target := fdTarget(fd)
	w.closeConn(fd)
	if fdTarget(fd) == target {
		t.Fatalf("fd %d kept open with nothing owed", fd)
	}
	if w.closeFDOwed != 0 || (len(w.pendingRelease) == 1 && w.pendingRelease[0].holdsFD) {
		t.Fatalf("a close with nothing owed registered a kept descriptor: closeFDOwed=%d %+v", w.closeFDOwed, w.pendingRelease)
	}
	if !peerSawEnd(peer, time.Second) {
		t.Error("the peer saw no end of the connection")
	}
}

// TestShutdownEndsOwedOpsBeforeClosing drives worker shutdown with idle
// keep-alive connections, each with its next recv armed (linked behind its
// last response's SEND), so every one has an op owed at shutdown. Shutdown
// must end those ops and then close every descriptor: none may be left open,
// the engine must stop promptly, and the release backstop must not be what
// closed any of them.
func TestShutdownEndsOwedOpsBeforeClosing(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	e, err := New(resource.Config{
		Addr:      addr,
		Protocol:  engine.HTTP1,
		Resources: resource.Resources{Workers: 2},
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, transplantTestHandler{})
	if err != nil {
		skipOrFail656(t, "iouring engine unavailable: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Listen(ctx) }()
	defer cancel()
	for deadline := time.Now().Add(8 * time.Second); e.Addr() == nil; {
		select {
		case err := <-done:
			skipOrFail656(t, "iouring engine failed to start: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("engine did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	const n = 8
	var clients []net.Conn
	var servers []int
	var targets []string
	for i := range n {
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		clients = append(clients, c)
		_ = c.SetDeadline(time.Now().Add(3 * time.Second))
		if _, err := c.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		buf := make([]byte, 512)
		var got []byte
		for !strings.Contains(string(got), "\r\n\r\nok") {
			k, err := c.Read(buf)
			got = append(got, buf[:k]...)
			if err != nil {
				t.Fatalf("read %d: %v (%q)", i, err, got)
			}
		}
		srv := -1
		for end := time.Now().Add(time.Second); srv < 0 && time.Now().Before(end); {
			srv = serverFDFor(c.LocalAddr().String())
		}
		if srv < 0 {
			t.Fatalf("conn %d: no server-side descriptor found", i)
		}
		servers = append(servers, srv)
		targets = append(targets, fdTarget(srv))
	}
	defer func() {
		for _, c := range clients {
			_ = c.Close()
		}
	}()
	start := time.Now()
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the engine did not stop within 3s")
	}
	took := time.Since(start)
	left := 0
	for i, fd := range servers {
		if fdTarget(fd) == targets[i] {
			left++
		}
	}
	m := e.Metrics()
	t.Logf("celeris685 shutdown conns=%d took=%v left_open=%d close_fd_forced=%d", n, took, left, m.CloseFDForced)
	if left != 0 {
		t.Fatalf("%d of %d server-side descriptors still open after the engine stopped", left, n)
	}
	if m.CloseFDForced != 0 {
		t.Fatalf("CloseFDForced = %d, want 0", m.CloseFDForced)
	}
}

// The SEND_ZC half of the rule (celeris#798). A SEND_ZC completes in two
// CQEs: the send's result (IORING_CQE_F_MORE), then a notification once the
// kernel has let go of the send buffer's pages. The notification names no
// descriptor, so no op can resolve the number through it; it guards the send
// buffer, which the connState holds. But a peer that stops reading keeps the
// unsent part of the send queued, and the notification with it, for as long
// as the socket is open, and keeping the descriptor keeps the socket open. A
// close that counted the notification as an op owed on the descriptor kept
// it until the 5 s release backstop forced it (CloseFDForced, which must
// stay 0), and worker shutdown waited out its whole drain bound for it.

// zcReleaseBound is how soon after the close the descriptor must be closed
// when all that holds the connection is a SEND_ZC notification and ops that
// end at the close's cancel and shutdown. The rule closes it at the close
// itself when no owed op names it, and otherwise at the drainPendingRelease
// of the first loop pass that reads the last naming op's terminal CQE (a
// cancelled recv) or the send's first CQE: one pass, and a pass here waits
// at most 10 ms for completions. 200 ms leaves 20 passes of headroom for
// -race on a 4-CPU container, and is 25 times shorter than the 5 s backstop
// (pendingReleaseHoldNanos) the descriptor was held to.
const zcReleaseBound = 200 * time.Millisecond

// zcShutdownBound is the same for worker shutdown's drain: with only a
// notification owed it does not drain at all, and with the send's first CQE
// already in the ring it stops at the first pass, which returns at once.
// Held for the notification, the drain ran its whole 250 ms bound
// (shutdownFDDrainNanos); 100 ms sits well between the two.
const zcShutdownBound = 100 * time.Millisecond

// zcPayload is the send: 64 KiB, far over sendZCMinBytes (so it goes out as
// SEND_ZC) and over what a peer with a 4 KiB receive buffer can take, so what
// the send queued past the peer's window stays in the server's send queue and
// holds the notification. Small enough that the pages SEND_ZC charges to
// RLIMIT_MEMLOCK fit the CI unit job's 8 MiB next to the test ring.
const zcPayload = 64 << 10

// newZCCloseWorker is newOwedCloseWorker over loopback TCP, with SEND_ZC on
// as the engine turns it on: the startup probe must find it functional, and
// CELERIS_IOURING_SEND_ZC=on then enables it (resolveSendZCPolicy); a kernel
// where the engine never sends zero-copy cannot reach the case. The peer has
// a 4 KiB receive buffer, set before the connect so that the window it
// advertises is small from the first segment, and reads nothing until
// drainZCPeer.
func newZCCloseWorker(t *testing.T) (*Worker, *connState, int) {
	t.Helper()
	res, reason := probeSendZCCached()
	if on, _ := resolveSendZCPolicy(res == SendZCTrueZeroCopy || res == SendZCCopyFallback, "on"); !on {
		t.Skipf("the engine does not turn SEND_ZC on here (probe: %v %s)", res, reason)
	}
	ring := newTestRing(t)
	lfd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("listen socket: %v", err)
	}
	defer func() { _ = unix.Close(lfd) }()
	if err := unix.Bind(lfd, &unix.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if err := unix.Listen(lfd, 1); err != nil {
		t.Fatalf("listen: %v", err)
	}
	sa, err := unix.Getsockname(lfd)
	if err != nil {
		t.Fatalf("getsockname: %v", err)
	}
	peer, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("peer socket: %v", err)
	}
	if err := unix.SetsockoptInt(peer, unix.SOL_SOCKET, unix.SO_RCVBUF, 4096); err != nil {
		_ = unix.Close(peer)
		t.Fatalf("SO_RCVBUF: %v", err)
	}
	if err := unix.Connect(peer, sa); err != nil {
		_ = unix.Close(peer)
		t.Fatalf("connect: %v", err)
	}
	local, _, err := unix.Accept4(lfd, unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC)
	if err != nil {
		_ = unix.Close(peer)
		t.Fatalf("accept: %v", err)
	}
	localTarget := fdTarget(local)
	t.Cleanup(func() {
		_ = unix.Close(peer)
		if fdTarget(local) == localTarget {
			_ = unix.Close(local)
		}
	})
	w := &Worker{
		ring:        ring,
		conns:       make([]*connState, local+1),
		liveConns:   make([]int, 0, 4),
		errs:        &errclass.Counters{},
		activeConns: &atomic.Int64{},
		closeCount:  &atomic.Uint64{},
		recvArm:     &recvArmStats{},
		handoffLoss: &handoffLossStats{},
		sendZC:      true,
	}
	w.cachedNow = time.Now().UnixNano()
	cs := &connState{
		fd:         local,
		liveIdx:    -1,
		generation: 13,
		buf:        make([]byte, 4096),
		h1State:    conn.NewH1State(),
		detected:   true,
	}
	cs.protocol.Store(int32(engine.HTTP1))
	w.conns[local] = cs
	w.addLiveConn(cs)
	w.connCount = 1
	w.activeConns.Add(1)
	return w, cs, peer
}

// startZCSend places one SEND_ZC of zcPayload bytes on cs, submits it, and
// returns what it sent once its first CQE (the send's result, F_MORE) is in
// the completion ring. With reap it then dispatches that CQE as the loop does
// (staleConnCQE, then handleSend, which records zcNotifPending); without, it
// leaves it there unread, as a close that runs before the loop reads it finds
// it. Either way the notification must still be owed 100 ms later, with the
// peer reading nothing, or the case did not form.
func startZCSend(t *testing.T, w *Worker, cs *connState, reap bool) int32 {
	t.Helper()
	payload := make([]byte, zcPayload)
	for i := range payload {
		payload[i] = byte(i)
	}
	cs.writeBuf = payload
	if w.flushSend(cs) || !cs.sendIsZC || cs.kernelInflight != 1 {
		t.Fatalf("no SEND_ZC placed: sendIsZC=%v kernelInflight=%d", cs.sendIsZC, cs.kernelInflight)
	}
	if _, err := w.ring.Submit(); err != nil {
		t.Fatalf("submit: %v", err)
	}
	var c *completionEntry
	for end := time.Now().Add(2 * time.Second); c == nil; {
		if head, tail := w.ring.BeginCQ(); head != tail {
			c = w.ring.cqeAt(head)
			break
		}
		if time.Now().After(end) {
			t.Fatal("no completion for the SEND_ZC within 2s")
		}
		time.Sleep(time.Millisecond)
	}
	if c.UserData&udMask != udSend || !cqeHasMore(c.Flags) || c.Res <= 0 {
		t.Fatalf("first completion ud=%#x flags=%#x res=%d, want the SEND_ZC's result (F_MORE, res > 0)", c.UserData, c.Flags, c.Res)
	}
	sent := c.Res
	if reap {
		head, _ := w.ring.BeginCQ()
		if !w.staleConnCQE(c, cs.fd, c.UserData) {
			w.handleSend(c, cs.fd, time.Now().UnixNano())
		}
		w.ring.EndCQ(head + 1)
		if !cs.zcNotifPending || !cs.sending || cs.kernelInflight != 1 {
			t.Fatalf("after the first CQE: zcNotifPending=%v sending=%v kernelInflight=%d, want true true 1", cs.zcNotifPending, cs.sending, cs.kernelInflight)
		}
	}
	time.Sleep(100 * time.Millisecond)
	want := uint32(1)
	if reap {
		want = 0
	}
	if head, tail := w.ring.BeginCQ(); tail-head != want {
		t.Fatalf("%d completions in the ring 100 ms after the send, want %d: the notification arrived with the peer "+
			"reading nothing (sent %d of %d), so the case did not form", tail-head, want, sent, zcPayload)
	}
	return sent
}

// sweepClose closes fd as a connection with a send still owed is closed:
// closeConn defers it (cs.closing), and the closing-drain sweep in
// checkTimeouts reaps it once the peer has read nothing for
// closingDrainTimeoutNanos. The sweep's two calls, without the 5 s wait.
func sweepClose(t *testing.T, w *Worker, cs *connState) {
	t.Helper()
	fd := cs.fd
	w.closeConn(fd)
	if w.conns[fd] != cs || !cs.closing {
		t.Fatalf("closeConn did not defer the close of a conn with a send owed: registered=%v closing=%v", w.conns[fd] == cs, cs.closing)
	}
	w.removeDirty(cs)
	w.finishCloseAny(fd, cs)
	if w.conns[fd] != nil {
		t.Fatal("the conn is still registered after the sweep's close")
	}
}

// drainZCPeer lets the peer read everything to EOF while the loop runs, and
// reports what it read and whether the notification arrived (cs's last op
// retired while cs was still queued for release) before cs was released.
func drainZCPeer(t *testing.T, w *Worker, cs *connState, peer int) (got int, eof, notified bool) {
	t.Helper()
	if err := unix.SetNonblock(peer, true); err != nil {
		t.Fatalf("peer nonblock: %v", err)
	}
	buf := make([]byte, 64<<10)
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); {
		for !eof {
			n, err := unix.Read(peer, buf)
			if n > 0 {
				got += n
				continue
			}
			if n == 0 && err == nil {
				eof = true
			}
			break
		}
		runRingOnce(t, w, 5*time.Millisecond)
		if len(w.pendingRelease) == 1 && w.pendingRelease[0].cs == cs && cs.kernelInflight == 0 {
			notified = true
		}
		w.cachedNow = time.Now().UnixNano()
		w.drainPendingRelease()
		if eof && len(w.pendingRelease) == 0 {
			break
		}
	}
	return got, eof, notified
}

// TestCloseReleasesDescriptorWithOnlyAZCNotificationOwed is celeris#798 on the
// close paths. A connection sent a SEND_ZC to a peer that stopped reading, so
// the notification stays owed, and was closed by the closing-drain sweep:
//
//   - notif-only: the send's first CQE was read; nothing else is owed. No
//     op names the descriptor, so it must be closed at the close.
//   - recv-and-notif: a recv was armed after the send too (the keep-alive
//     path arms one behind every unlinked send). The descriptor is kept for
//     the recv, which the close's cancel and shutdown end at once, and must
//     then be closed although the notification is still owed.
//   - send-done-after-close: the send's first CQE was in the ring, unread,
//     when the close ran, so the close kept the descriptor for the send. The
//     CQE says the send is done, and the descriptor must then be closed.
//
// In each, the descriptor must be closed within zcReleaseBound of the close
// and CloseFDForced must stay 0. The send buffer must not go with it: the
// connState, whose sendBuf the kernel may still read, must stay queued until
// the notification arrives (here, once the peer has read everything), and
// be released then, by the notification and not by the backstop.
func TestCloseReleasesDescriptorWithOnlyAZCNotificationOwed(t *testing.T) {
	for _, tc := range []struct {
		name       string
		reap, recv bool
	}{
		{"notif-only", true, false},
		{"recv-and-notif", true, true},
		{"send-done-after-close", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			w, cs, peer := newZCCloseWorker(t)
			fd := cs.fd
			target := fdTarget(fd)
			sent := startZCSend(t, w, cs, tc.reap)
			if tc.recv {
				if !w.prepareRecv(cs, cs.buf) || cs.kernelInflight != 2 {
					t.Fatalf("recv not placed: kernelInflight=%d", cs.kernelInflight)
				}
				if res := runRingOnce(t, w, 10*time.Millisecond); len(res) != 0 {
					t.Fatalf("the recv completed before the close (%v): the peer sent nothing", res)
				}
			}
			closedAt := time.Now()
			sweepClose(t, w, cs)
			var releasedAfter time.Duration
			for {
				if fdTarget(fd) != target {
					releasedAfter = time.Since(closedAt)
					break
				}
				if time.Since(closedAt) > 6*time.Second {
					t.Fatalf("fd %d still open 6s after the close", fd)
				}
				runRingOnce(t, w, 10*time.Millisecond)
				w.cachedNow = time.Now().UnixNano()
				w.drainPendingRelease()
			}
			forced := w.handoffLoss.closeFDForced.Load()
			t.Logf("celeris798 close case=%s sent=%d released_after=%v close_fd_forced=%d", tc.name, sent, releasedAfter.Round(time.Microsecond), forced)
			if releasedAfter > zcReleaseBound || forced != 0 {
				t.Fatalf("fd %d was closed %v after the close (bound %v) with CloseFDForced=%d: a pending SEND_ZC "+
					"notification, which names no descriptor, held it", fd, releasedAfter, zcReleaseBound, forced)
			}
			if w.closeFDOwed != 0 {
				t.Fatalf("closeFDOwed=%d after the descriptor was closed, want 0", w.closeFDOwed)
			}
			// The send buffer: cs stays queued, holding sendBuf, while the
			// notification is owed, however often the release runs.
			for end := time.Now().Add(100 * time.Millisecond); time.Now().Before(end); {
				runRingOnce(t, w, 10*time.Millisecond)
				w.cachedNow = time.Now().UnixNano()
				w.drainPendingRelease()
			}
			if len(w.pendingRelease) != 1 || w.pendingRelease[0].cs != cs || w.pendingRelease[0].holdsFD || cs.kernelInflight == 0 || cs.fd != fd {
				t.Fatalf("with the notification still owed the connState must stay queued for release without "+
					"its descriptor: pendingRelease=%+v kernelInflight=%d", w.pendingRelease, cs.kernelInflight)
			}
			got, eof, notified := drainZCPeer(t, w, cs, peer)
			t.Logf("celeris798 close case=%s peer_got=%d eof=%v notified=%v released=%v", tc.name, got, eof, notified, len(w.pendingRelease) == 0)
			if !eof || got != int(sent) {
				t.Fatalf("the peer read %d bytes (EOF %v), want the %d the send completed with and then EOF", got, eof, sent)
			}
			if !notified || len(w.pendingRelease) != 0 {
				t.Fatalf("the connState was not released at the notification: notified=%v pendingRelease=%d", notified, len(w.pendingRelease))
			}
			if forced := w.handoffLoss.closeFDForced.Load(); forced != 0 {
				t.Fatalf("CloseFDForced = %d, want 0", forced)
			}
		})
	}
}

// TestShutdownDoesNotWaitForAZCNotification is celeris#798 at worker
// shutdown: endOwedOpsAtShutdown waits for the ops that name a live
// connection's descriptor, and a SEND_ZC notification names none. With the
// notification owed after the send's first CQE was read (notif-pending), or
// with that CQE still in the ring when the drain starts
// (send-done-during-drain), the drain must end within zcShutdownBound rather
// than run out its 250 ms bound.
func TestShutdownDoesNotWaitForAZCNotification(t *testing.T) {
	for _, tc := range []struct {
		name string
		reap bool
	}{
		{"notif-pending", true},
		{"send-done-during-drain", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			w, cs, _ := newZCCloseWorker(t)
			sent := startZCSend(t, w, cs, tc.reap)
			start := time.Now()
			w.endOwedOpsAtShutdown()
			took := time.Since(start)
			t.Logf("celeris798 shutdown case=%s sent=%d took=%v kernelInflight_after=%d zcNotifPending_after=%v", tc.name, sent, took.Round(time.Microsecond), cs.kernelInflight, cs.zcNotifPending)
			if took > zcShutdownBound {
				t.Fatalf("endOwedOpsAtShutdown took %v (bound %v) with only a SEND_ZC notification owed", took, zcShutdownBound)
			}
			if w.closeFDOwed != 0 {
				t.Fatalf("closeFDOwed=%d after shutdown's drain, want 0", w.closeFDOwed)
			}
		})
	}
}
