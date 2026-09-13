//go:build linux

package sockopts

import (
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestDrainRecvBufferNonBlocking is the regression guard for celeris#311.
//
// DrainRecvBuffer runs on the io_uring event-loop worker thread during the
// graceful detached-close path (SHUT_WR → drain → close). Those sockets are in
// blocking mode, so the original unix.Read implementation blocked forever when
// a connection was closed before the peer's FIN arrived (the churn-close race),
// wedging the worker — and with enough concurrent detached closes the whole
// engine stopped servicing requests.
//
// This test puts a blocking-mode socket in exactly that state (empty receive
// buffer, peer still open so no FIN) and asserts the drain returns promptly.
// With a blocking Read it hangs and trips the timeout; with MSG_DONTWAIT it
// returns immediately.
func TestDrainRecvBufferNonBlocking(t *testing.T) {
	fds := socketpair(t)

	done := make(chan struct{})
	go func() {
		DrainRecvBuffer(fds[0])
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("DrainRecvBuffer blocked on an empty blocking socket (celeris#311 regression)")
	}
}

// TestDrainRecvBufferDrainsThenReturns verifies the drain still does its job:
// it consumes data already queued in the receive buffer (so close() does not
// RST away a staged GOAWAY / close frame) and then returns once the buffer is
// empty — without blocking on the still-open peer.
func TestDrainRecvBufferDrainsThenReturns(t *testing.T) {
	fds := socketpair(t)

	if _, err := unix.Write(fds[1], []byte("queued bytes the drain must discard")); err != nil {
		t.Fatalf("write: %v", err)
	}

	done := make(chan struct{})
	go func() {
		DrainRecvBuffer(fds[0])
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("DrainRecvBuffer blocked after draining queued data (celeris#311 regression)")
	}

	var buf [64]byte
	n, _, rerr := unix.Recvfrom(fds[0], buf[:], unix.MSG_DONTWAIT)
	if n > 0 {
		t.Fatalf("DrainRecvBuffer left %d bytes undrained", n)
	}
	if rerr != unix.EAGAIN && rerr != unix.EWOULDBLOCK {
		t.Fatalf("expected EAGAIN after drain, got n=%d err=%v", n, rerr)
	}
}

// TestDrainRecvBufferDrainsPastTheOldFixedCap is the regression guard for
// celeris#569 reopened: the drain used to stop at a fixed 32 KiB, which is
// below the autotuned receive buffer, so on the close it exists for (a
// recv-paused connection with ~128 KiB queued) it left the queue non-empty
// and close(2) reset anyway. The drain now runs until a read finds the queue
// empty, so it consumes the whole backlog however far past the old cap it is.
func TestDrainRecvBufferDrainsPastTheOldFixedCap(t *testing.T) {
	fds := socketpair(t)

	queued := fillSocket(t, fds[1], 4*drainRecvFallbackBudget)
	if queued <= drainRecvFallbackBudget {
		t.Skipf("socket buffer held only %d bytes, not more than the %d-byte old cap", queued, drainRecvFallbackBudget)
	}

	done := make(chan int, 1)
	go func() { done <- DrainRecvBuffer(fds[0]) }()
	var drained int
	select {
	case drained = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("DrainRecvBuffer did not return")
	}
	if drained != queued {
		t.Fatalf("drain consumed %d of the %d queued bytes: the fixed cap is back (celeris#569)", drained, queued)
	}
	var buf [64]byte
	n, _, rerr := unix.Recvfrom(fds[0], buf[:], unix.MSG_DONTWAIT)
	if n > 0 {
		t.Fatalf("DrainRecvBuffer left %d bytes queued: close(2) would still RST (celeris#569)", n)
	}
	if rerr != unix.EAGAIN && rerr != unix.EWOULDBLOCK {
		t.Fatalf("expected EAGAIN after drain, got n=%d err=%v", n, rerr)
	}
}

// TestDrainRecvBufferByteBoundIsComputedOnce is the regression guard for the
// byte bound, and the reason it is a socketpair of DATAGRAMS.
//
// The bound is min(SIOCINQ, SO_RCVBUF) fixed before the first read. What must
// not happen is re-snapshotting it after a read: then a peer that keeps
// delivering extends the work round after round and only the wall clock ever
// stops it, which is celeris#571's complaint. Telling the two apart needs a
// queue that still holds bytes after the entry snapshot has been drained —
// and on a stream socket that means racing a writer, which on this kernel the
// drain simply outruns (the sibling test below measures it ending on EAGAIN at
// 64 KiB in ~17 us, so it cannot discriminate).
//
// A SOCK_DGRAM AF_UNIX socket gives that state with no race at all: Linux's
// unix_inq_len() sums the receive queue only for SOCK_STREAM and
// SOCK_SEQPACKET, and for a datagram socket returns the length of the FIRST
// datagram alone. Three 4 KiB datagrams therefore queue 12 KiB behind a
// SIOCINQ that says 4096. A drain whose budget is computed once consumes 4096
// and leaves the rest; one that re-snapshots consumes all 12288. Measured
// both ways: 4096/inq_after=4096 here, 12288/inq_after=0 against the
// pre-correction loop.
func TestDrainRecvBufferByteBoundIsComputedOnce(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_DGRAM, 0)
	if err != nil {
		t.Fatalf("socketpair(SOCK_DGRAM): %v", err)
	}
	t.Cleanup(func() {
		_ = unix.Close(fds[0])
		_ = unix.Close(fds[1])
	})

	const datagrams = 3
	chunk := make([]byte, drainRecvBufSize)
	for i := range datagrams {
		if _, err := unix.Write(fds[1], chunk); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	inq, err := unix.IoctlGetInt(fds[0], unix.SIOCINQ)
	if err != nil {
		t.Skipf("SIOCINQ on a datagram socket: %v", err)
	}
	if inq != drainRecvBufSize {
		t.Skipf("SIOCINQ reports %d, not the single-datagram %d this kernel is assumed to report", inq, drainRecvBufSize)
	}

	drained := DrainRecvBuffer(fds[0])
	inqAfter, _ := unix.IoctlGetInt(fds[0], unix.SIOCINQ)
	t.Logf("dgram byte bound: entry SIOCINQ=%d queued=%d drained=%d inq_after=%d", inq, datagrams*drainRecvBufSize, drained, inqAfter)
	if drained != inq {
		t.Fatalf("drain consumed %d bytes against an entry SIOCINQ of %d with %d queued: the byte bound is re-snapshotted per round, not computed once (celeris#571)",
			drained, inq, datagrams*drainRecvBufSize)
	}
}

// TestDrainRecvBufferStopsAtTheTimeBudget is the end-to-end guard for
// celeris#571: a peer that keeps delivering bytes as fast as the drain
// consumes them must not hold the event-loop thread. Two bounds fixed before
// the first read hold the call — min(SIOCINQ, SO_RCVBUF) bytes and
// drainRecvTimeBudget of wall clock — and neither can be extended by what the
// peer does afterwards.
//
// The writer here never stops on its own, so an unbounded drain never returns.
// The assertion is against drainRecvTimeBudget itself and not some round
// number: a 1 s bound would still pass if the budget regressed a thousandfold,
// which is what it used to assert.
//
// Which of the two bounds ends it is not asserted, because on this kernel the
// drain outruns a single writing goroutine and ends on EAGAIN inside the byte
// budget (~65 KiB in ~17 us, logged below). The bounds themselves are pinned
// deterministically by TestDrainRecvBufferByteBoundIsComputedOnce and
// TestDrainRecvRoundStopsAtItsDeadline; this one guards the call as a whole.
func TestDrainRecvBufferStopsAtTheTimeBudget(t *testing.T) {
	fds := socketpair(t)

	if queued := fillSocket(t, fds[1], 64<<10); queued == 0 {
		t.Skip("nothing could be queued")
	}
	rcvbuf, err := unix.GetsockoptInt(fds[0], unix.SOL_SOCKET, unix.SO_RCVBUF)
	if err != nil {
		t.Fatalf("SO_RCVBUF: %v", err)
	}
	stopFlood := floodPeer(t, fds[1])

	done := make(chan int, 1)
	start := time.Now()
	go func() { done <- DrainRecvBuffer(fds[0]) }()
	var drained int
	select {
	case drained = <-done:
	case <-time.After(10 * time.Second):
		stopFlood()
		t.Fatal("DrainRecvBuffer chased the writing peer instead of stopping at its budget (celeris#571)")
	}
	elapsed := time.Since(start)
	stopFlood()
	// The slack is for the goroutine handoff and the one read the deadline
	// check cannot preempt, not for the budget: 20x a 1 ms budget still fails
	// long before a regression to the old between-rounds-only bound would.
	if limit := 20 * drainRecvTimeBudget; elapsed > limit {
		t.Fatalf("drain against a non-stopping peer took %s, past %dx the %s budget (celeris#571)", elapsed, int64(limit/drainRecvTimeBudget), drainRecvTimeBudget)
	}
	// The byte bound holds end to end too: whatever the peer did during the
	// call, the drain never read past the buffer that queue lives in.
	if drained > rcvbuf {
		t.Fatalf("drain consumed %d bytes with SO_RCVBUF=%d: it read past the buffer the queue lives in (celeris#571)", drained, rcvbuf)
	}
	t.Logf("drain returned after %s having consumed %d bytes (rcvbuf %d, time budget %s)", elapsed, drained, rcvbuf, drainRecvTimeBudget)
}

// TestDrainRecvRoundStopsAtItsByteBudget checks the byte bound: a round never
// reads past the budget it was given, whatever is queued behind it.
func TestDrainRecvRoundStopsAtItsByteBudget(t *testing.T) {
	fds := socketpair(t)

	queued := fillSocket(t, fds[1], 64<<10)
	if queued <= 8192 {
		t.Skipf("socket buffer held only %d bytes", queued)
	}

	buf := make([]byte, drainRecvBufSize)
	drained, empty := drainRecvRound(fds[0], buf, 8192, time.Time{})
	if drained != 8192 || empty {
		t.Fatalf("round consumed %d bytes (empty=%t), want exactly 8192 with the queue not empty", drained, empty)
	}
	left, _, _ := unix.Recvfrom(fds[0], buf, unix.MSG_DONTWAIT)
	if left <= 0 {
		t.Fatalf("round consumed the whole %d-byte backlog instead of its 8192-byte budget", queued)
	}
}

// TestDrainRecvRoundStopsAtItsDeadline is the guard on WHERE the deadline is
// checked. It used to be checked only BETWEEN rounds, so a round with a large
// budget could read a whole SO_RCVBUF past it; the check now sits between the
// reads inside the round.
//
// The first half is the deterministic one: a queue that is full, a budget far
// bigger than it, and a deadline that has already passed. A round that checks
// its deadline only on the way out reads the entire backlog here; one that
// checks it between reads reads nothing at all. The second half is the timing
// guard with a live peer.
func TestDrainRecvRoundStopsAtItsDeadline(t *testing.T) {
	fds := socketpair(t)

	queued := fillSocket(t, fds[1], 1<<20)
	if queued <= drainRecvBufSize {
		t.Skipf("socket buffer held only %d bytes", queued)
	}

	buf := make([]byte, drainRecvBufSize)
	expired := time.Now().Add(-time.Millisecond)
	drained, empty := drainRecvRound(fds[0], buf, 1<<30, expired)
	if drained != 0 || empty {
		t.Fatalf("round consumed %d of the %d queued bytes (empty=%t) with its deadline already past: the deadline is not checked inside the round (celeris#571)", drained, queued, empty)
	}

	// Same round against a peer that never stops, with a budget it could not
	// exhaust: whatever ends it, it must end within a small multiple of the
	// budget, not of some round number.
	defer floodPeer(t, fds[1])()

	start := time.Now()
	drained, empty = drainRecvRound(fds[0], buf, 1<<30, start.Add(drainRecvTimeBudget))
	elapsed := time.Since(start)
	if limit := 20 * drainRecvTimeBudget; elapsed > limit {
		t.Fatalf("one round ran %s against a %s deadline (celeris#571)", elapsed, drainRecvTimeBudget)
	}
	if drained >= 1<<30 {
		t.Fatalf("round consumed its whole %d-byte budget", 1<<30)
	}
	t.Logf("round returned after %s having consumed %d bytes (empty=%t) against a %s deadline", elapsed, drained, empty, drainRecvTimeBudget)
}

// floodPeer keeps writing into fd until the returned stop is called, which
// waits for the writer to be gone before returning. It is the "peer that never
// stops delivering" the bounds exist for (celeris#571).
//
// The writes are NON-BLOCKING and EAGAIN is not an error here. A blocking
// writer parks inside write(2) as soon as the reader stops draining and never
// looks at its stop channel again, so the WaitGroup the test waits on is never
// released: that wedged TestDrainRecvBufferStopsAtTheTimeBudget for the full
// 20-minute -race timeout, with the writer's goroutine stuck in
// syscall.Syscall(write). A non-blocking writer reaches the stop check on
// every iteration whatever the queue is doing.
func floodPeer(t *testing.T, fd int) func() {
	t.Helper()
	if err := unix.SetNonblock(fd, true); err != nil {
		t.Fatalf("setnonblock: %v", err)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		chunk := make([]byte, drainRecvBufSize)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := unix.Write(fd, chunk); err != nil && err != unix.EAGAIN && err != unix.EWOULDBLOCK {
				return
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			close(stop)
			wg.Wait()
			_ = unix.SetNonblock(fd, false)
		})
	}
}

// fillSocket writes up to want bytes into fd without blocking and returns
// what the kernel accepted.
func fillSocket(t *testing.T, fd int, want int) int {
	t.Helper()
	if err := unix.SetNonblock(fd, true); err != nil {
		t.Fatalf("setnonblock: %v", err)
	}
	defer func() {
		if err := unix.SetNonblock(fd, false); err != nil {
			t.Fatalf("setnonblock(false): %v", err)
		}
	}()
	chunk := make([]byte, 4096)
	queued := 0
	for queued < want {
		n, err := unix.Write(fd, chunk)
		if n > 0 {
			queued += n
		}
		if err != nil {
			break
		}
	}
	return queued
}

func socketpair(t *testing.T) [2]int {
	t.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	t.Cleanup(func() {
		_ = unix.Close(fds[0])
		_ = unix.Close(fds[1])
	})
	return [2]int{fds[0], fds[1]}
}
