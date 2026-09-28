//go:build linux && validation

package iouring

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/internal/recvtheft"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"
)

// celeris#685, the linked form of the celeris#715 theft, made deterministic.
//
// A sync-mode HTTP/1 response is flushed with its connection's next recv
// chained behind the SEND (flushSendLink, IOSQE_IO_LINK). The kernel consumes
// both SQEs at the submit, but it issues the recv, and resolves its descriptor
// NUMBER, only when the SEND completes: on a DEFER_TASKRUN ring as task work,
// which the enter that posts the SEND's CQE can leave queued. A close path
// that runs at that CQE (completeSend of a connection closeConn deferred while
// the SEND was in flight) queues a cancel that cannot find the recv (it is in
// no cancel table yet) and closed the descriptor at once. If a sibling worker
// is given the number before the closer's next enter, that enter issues the
// recv against the new connection's socket.
//
// The trial:
//  1. A sends half a request (TCP_DEFER_ACCEPT holds the accept until it
//     does). The test finds A's server-side descriptor N and fills its send
//     queue from outside the engine (A never reads), so the next SEND cannot
//     complete.
//  2. A sends the rest. The worker W1 answers with a linked SEND+recv; the
//     SEND waits for room.
//  3. WriteTimeout fires (once A's header timer has woken W1, see
//     linkedHeaderTimeout): checkTimeouts' closeConn finds the SEND in
//     flight and defers the close.
//  4. A drains its socket. The SEND completes, and completeSend runs the
//     deferred close with the linked recv owed; W1 parks when the close path
//     returns (recvtheft.Options.LinkedRecv).
//  5. The test dials B's candidates. The sibling W2 accepts one, as N if the
//     close released it (and W2 then parks before submitting B's recv).
//  6. W1 is released (its next enter runs the queued task work, which issues
//     the linked recv), then W2.
//
// Asserts what the fix restores: B answered, no stale recv carrying B's
// bytes, and a number kept open at the close released after. Fails on a tree
// that closes the number at once whenever the kernel left the recv unissued
// at the SEND's CQE (see TestRecvTheft685Linked's log: reused, stolen).
// Needs two io_uring workers and CELERIS_RECV_THEFT_715=1, like the #715 arms.

type linkedTheftHandler struct{}

func (linkedTheftHandler) HandleStream(_ context.Context, s *stream.Stream) error {
	if s.ResponseWriter == nil {
		return nil
	}
	return s.ResponseWriter.WriteResponse(s, 200,
		[][2]string{{"content-type", "text/plain"}, {"content-length", "2"}}, []byte("ok"))
}

const (
	linkedWriteTimeout = 200 * time.Millisecond
	// linkedHeaderTimeout is what wakes W1 once its SEND is blocked. A
	// worker whose only op is that SEND waits in the ring with no timeout
	// (run's mode 3a: a SEND is expected to complete), so its timeout sweep
	// does not run until some completion arrives. A's header timer, armed
	// by its half request, is that completion: it fires this long after A's
	// first bytes, finds the headers complete and does nothing, and from
	// the next iteration on the worker waits with a timeout and sweeps.
	linkedHeaderTimeout = 1 * time.Second
	// linkedCloseWait covers the header timer, WriteTimeout, and the timeout
	// sweep's cadence (every 32 iterations, each at most 25 ms with
	// ReadHeaderTimeout set).
	linkedCloseWait = 2500 * time.Millisecond
	linkedReqHead   = "GET /a HTTP/1.1\r\n"
	linkedReqTail   = "Host: recv-theft-685\r\n\r\n"
)

func startLinkedTheftEngine(t *testing.T) (*Engine, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick port: %v", err)
	}
	addr := ln.Addr().String()
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	e, err := New(resource.Config{
		Addr:              addr,
		Protocol:          engine.HTTP1,
		Resources:         resource.Resources{Workers: 2},
		WriteTimeout:      linkedWriteTimeout,
		ReadHeaderTimeout: linkedHeaderTimeout,
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, linkedTheftHandler{})
	if err != nil {
		skipOrFail656(t, "iouring engine unavailable: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Listen(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("engine did not stop within 5s")
		}
	})
	for deadline := time.Now().Add(8 * time.Second); e.Addr() == nil; {
		select {
		case err := <-done:
			skipOrFail656(t, "iouring engine failed to start: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("engine did not start listening within 8s")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n := e.NumWorkers(); n < 2 {
		skipOrFail656(t, "celeris#685 needs a sibling io_uring worker: workers=%d (RLIMIT_MEMLOCK funds one per 12 MiB)", n)
	}
	return e, port
}

// fillSendQueue writes to fd (a server-side socket whose peer never reads)
// until two passes 50 ms apart add nothing: the peer's window is closed and
// the send queue is full. Returns the bytes written.
func fillSendQueue(fd int) int {
	junk := make([]byte, 64<<10)
	total, idle := 0, 0
	for idle < 2 {
		wrote := 0
		for {
			n, err := unix.SendmsgN(fd, junk, nil, nil, unix.MSG_DONTWAIT|unix.MSG_NOSIGNAL)
			if n > 0 {
				wrote += n
			}
			if err != nil {
				break
			}
		}
		total += wrote
		if wrote == 0 {
			idle++
		} else {
			idle = 0
		}
		time.Sleep(50 * time.Millisecond)
	}
	return total
}

type linkedTheftResult struct {
	attempts          int
	hit               bool
	fd, closer, sibl  int
	bFD               int
	reused            bool
	heldOpen          bool
	released          bool
	filled            int
	witness           uint64
	staleClosed       uint64
	stale             []recvtheft.StaleRecv
	stolen            bool
	answered          bool
	answerErr         error
	missNoClose       int
	missQueued        int
	missCloserAccepts int
}

func linkedTheftAttempt(t *testing.T, e *Engine, sa *unix.SockaddrInet4, r *linkedTheftResult) bool {
	for deadline := time.Now().Add(3 * time.Second); e.metrics.activeConns.Load() != 0; {
		if time.Now().After(deadline) {
			t.Fatalf("engine still holds %d connections from the previous attempt", e.metrics.activeConns.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
	fillFDHoles(t)
	var cands []int
	defer func() {
		for _, fd := range cands {
			_ = unix.Close(fd)
		}
	}()
	for range theftCandidates {
		fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
		if err != nil {
			t.Fatalf("socket: %v", err)
		}
		cands = append(cands, fd)
	}
	// A: a small receive buffer, so the fill below stays small.
	a, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("socket A: %v", err)
	}
	defer func() { _ = unix.Close(a) }()
	_ = unix.SetsockoptInt(a, unix.SOL_SOCKET, unix.SO_RCVBUF, 4096)

	witness0 := recvtheft.CloseWithLinkedRecv()
	stale0 := e.metrics.handoffLoss.staleRecvDataClosed.Load()
	tr := recvtheft.Arm(recvtheft.Options{LinkedRecv: true, HoldMax: 10 * time.Second})
	defer tr.Disarm()

	if err := unix.Connect(a, sa); err != nil {
		t.Fatalf("connect A: %v", err)
	}
	if _, err := unix.Write(a, []byte(linkedReqHead)); err != nil {
		t.Fatalf("write A head: %v", err)
	}
	aLocalSA, err := unix.Getsockname(a)
	if err != nil {
		t.Fatalf("getsockname A: %v", err)
	}
	aLocal := sockaddrString(aLocalSA)
	srv := -1
	for deadline := time.Now().Add(2 * time.Second); srv < 0 && time.Now().Before(deadline); {
		if srv = serverFDFor(aLocal); srv < 0 {
			time.Sleep(2 * time.Millisecond)
		}
	}
	if srv < 0 {
		t.Fatalf("A (%s) was not accepted within 2s", aLocal)
	}
	// The search's own /proc/self/fd reads can leave a hole under N (a read
	// that took the lowest number while the accept was still to come); fill
	// it, so the number A's close frees is again the lowest free one.
	fillFDHoles(t)
	r.filled = fillSendQueue(srv)
	if _, err := unix.Write(a, []byte(linkedReqTail)); err != nil {
		t.Fatalf("write A tail: %v", err)
	}
	// The response's SEND now waits for room, with the next recv linked
	// behind it; WriteTimeout's closeConn defers the close meanwhile.
	closes0 := e.metrics.closeCount.Load()
	time.Sleep(linkedCloseWait)
	outqBefore, _ := unix.IoctlGetInt(srv, unix.SIOCOUTQ)
	// Drain A until the close hold fires: the SEND completes, and the
	// deferred close runs at its CQE.
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		buf := make([]byte, 64<<10)
		tv := unix.NsecToTimeval(int64(20 * time.Millisecond))
		_ = unix.SetsockoptTimeval(a, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv)
		for end := time.Now().Add(3 * time.Second); time.Now().Before(end); {
			n, err := unix.Read(a, buf)
			if n == 0 && err == nil {
				return
			}
			if err != nil && !errors.Is(err, unix.EAGAIN) && !errors.Is(err, unix.EINTR) {
				return
			}
		}
	}()
	ce, ok := tr.WaitClose(4 * time.Second)
	if !ok {
		r.missNoClose++
		<-drained
		outqAfter, _ := unix.IoctlGetInt(srv, unix.SIOCOUTQ)
		t.Logf("RECVTHEFT685 linked attempt=%d miss=no-close-hold filled=%d outq_before_drain=%d outq_after=%d srv_after=%q closes=+%d witness=+%d",
			r.attempts, r.filled, outqBefore, outqAfter, fdTarget(srv), e.metrics.closeCount.Load()-closes0, recvtheft.CloseWithLinkedRecv()-witness0)
		return false
	}
	aTarget := fdTarget(ce.FD)
	r.heldOpen = strings.HasPrefix(aTarget, "socket:")

	var hit *recvtheft.AcceptEvent
	var hitFD int
	for i, fd := range cands {
		if err := unix.Connect(fd, sa); err != nil {
			t.Fatalf("connect candidate %d: %v", i, err)
		}
		if _, err := unix.Write(fd, []byte(theftRequestB)); err != nil {
			t.Fatalf("write candidate %d: %v", i, err)
		}
		ev, ok := tr.NextAccept(theftAcceptWait)
		if !ok {
			r.missQueued++
			continue
		}
		if ev.Worker == ce.Worker {
			r.missCloserAccepts++
			continue
		}
		ev0 := ev
		if ev.FD == ce.FD {
			held, ok := tr.WaitAcceptHeld(time.Second)
			if !ok {
				t.Fatalf("accept of fd %d by worker %d reported but its hold did not fire", ev.FD, ev.Worker)
			}
			ev0 = held
			r.reused = true
		}
		hit, hitFD = &ev0, fd
		break
	}
	if hit == nil {
		t.Logf("RECVTHEFT685 linked attempt=%d miss=no-sibling-accept fd=%d closer=%d queued=%d closer_accepts=%d",
			r.attempts, ce.FD, ce.Worker, r.missQueued, r.missCloserAccepts)
		tr.Disarm()
		<-drained
		return false
	}
	r.hit, r.fd, r.closer, r.sibl, r.bFD = true, ce.FD, ce.Worker, hit.Worker, hit.FD

	tr.ReleaseClose()
	for deadline := time.Now().Add(theftSubmitWait); time.Now().Before(deadline) && len(tr.Stale()) == 0; {
		time.Sleep(time.Millisecond)
	}
	tr.ReleaseAccept()
	answer, aerr := readHead(hitFD, theftAnswerWait)
	r.answered, r.answerErr = strings.HasPrefix(answer, "HTTP/1.1 200"), aerr
	for deadline := time.Now().Add(theftReleaseWait); ; {
		if cur := fdTarget(ce.FD); !r.heldOpen || cur != aTarget {
			r.released = true
			break
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	<-drained
	r.witness = recvtheft.CloseWithLinkedRecv() - witness0
	r.staleClosed = e.metrics.handoffLoss.staleRecvDataClosed.Load() - stale0
	r.stale = tr.Stale()
	for _, s := range r.stale {
		if s.FD == r.fd && int(s.Res) == len(theftRequestB) && len(s.Head) > 0 &&
			string(s.Head) == theftRequestB[:len(s.Head)] {
			r.stolen = true
		}
	}
	return true
}

// TestRecvTheft685Linked is the linked form's trial (see above). One trial
// per run; tally the --- lines of -count=N.
func TestRecvTheft685Linked(t *testing.T) {
	requireRecvTheft715(t)
	e, port := startLinkedTheftEngine(t)
	sa := &unix.SockaddrInet4{Port: port, Addr: [4]byte{127, 0, 0, 1}}
	var r linkedTheftResult
	for r.attempts < theftMaxAttempts {
		r.attempts++
		if linkedTheftAttempt(t, e, sa, &r) {
			break
		}
	}
	var heads []string
	for _, s := range r.stale {
		heads = append(heads, strconv.Quote(string(s.Head)))
	}
	errText := "<nil>"
	if r.answerErr != nil {
		errText = r.answerErr.Error()
	}
	t.Logf("RECVTHEFT685 linked result attempts=%d hit=%v fd=%d closer=%d sibling=%d b_fd=%d reused=%v held_open=%v released=%v filled=%d witness=+%d stale_recv_data_closed=+%d stolen=%v answered=%v answer_err=%q stale=[%s] misses{no_close=%d queued=%d closer_accepts=%d}",
		r.attempts, r.hit, r.fd, r.closer, r.sibl, r.bFD, r.reused, r.heldOpen, r.released, r.filled, r.witness, r.staleClosed,
		r.stolen, r.answered, errText, strings.Join(heads, " "), r.missNoClose, r.missQueued, r.missCloserAccepts)
	if !r.hit {
		skipOrFail656(t, "INCONCLUSIVE: no sibling accept while the closer was parked, in %d attempts", r.attempts)
	}
	if r.witness == 0 {
		t.Fatalf("the close hold fired but close_with_linked_recv did not move")
	}
	if r.stolen || !r.answered {
		t.Fatalf("B (fd %d on worker %d; A's number %d reused=%v, kept open at the close=%v) lost its request: stolen by A's linked recv=%v (stale_recv_data_closed +%d), answered=%v (%v)",
			r.bFD, r.sibl, r.fd, r.reused, r.heldOpen, r.stolen, r.staleClosed, r.answered, r.answerErr)
	}
	if !r.released {
		t.Fatalf("A's number %d, kept open at the close, still names A's socket %v after the closer's release: the kept descriptor leaked", r.fd, theftReleaseWait)
	}
}
