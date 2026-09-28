//go:build linux

package iouring

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/engine/internal/errclass"
	"github.com/goceleris/celeris/internal/conn"
	"github.com/goceleris/celeris/resource"
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
