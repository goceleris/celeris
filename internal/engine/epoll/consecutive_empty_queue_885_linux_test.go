//go:build linux

package epoll

import (
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/resource"
)

// celeris#885 item 42: while driverReadQ is non-empty the run loop waits in
// epoll_wait with timeout 0 (celeris#930), and the rounds that return no
// events still counted toward consecutiveEmpty, the count adaptiveTimeoutMs
// stretches the idle wait by (base*2 after 10, base*4 after 100). A flood of
// driver bytes that takes a dozen rounds to read therefore ended with the loop
// already counted as long idle, and its first idle waits started stretched.
// No correctness impact (an event ends a wait at once); the test is on the
// counter's effect: the wait the loop would make as the last of the queued
// reads is delivered.

// sndbuf885x42 is the send buffer asked for on the socketpair; the kernel caps
// it at wmem_max, which is 212992 on a stock host and 4 MiB on some others.
const sndbuf885x42 = 16 << 20

// TestQueuedZeroTimeoutRoundsDoNotStretchTheIdleWait885 fills a socketpair
// with as much as it takes (16 MiB asked for; a kernel with a small wmem_max
// gives a few hundred KiB), so the loop reads it over several rounds: a turn is
// driverReadBudget reads of driverReadBufSize (256 KiB), and every round after
// the first is an epoll_wait(0) that collects nothing. The engine has
// ReadHeaderTimeout -1 and its 25 ms timerfd disarmed, so that nothing can end
// the flood's counting between two rounds. On the last read the count is 0 (round 1 had an event), and with
// more than eleven turns the wait the loop would make next (adaptiveTimeoutMs)
// is the base, 1 ms: the rounds a queue forced to zero are not idle rounds.
func TestQueuedZeroTimeoutRoundsDoNotStretchTheIdleWait885(t *testing.T) {
	eng, _ := startE2E885(t, false, newPathHandler885(1), func(c *resource.Config) { c.ReadHeaderTimeout = -1 })
	l := eng.WorkerLoop(0).(*Loop)
	if l.timerFD < 0 {
		t.Fatalf("apparatus: the loop has no sweep timerfd to disarm (every loop has one, celeris#1194)")
	}
	// The timerfd can fire between two rounds of the flood: disarm it.
	if err := unix.TimerfdSettime(l.timerFD, 0, &unix.ItimerSpec{}, nil); err != nil {
		t.Fatalf("apparatus: disarm the sweep timerfd: %v", err)
	}
	a, peer := socketpairNonblocking(t)
	t.Cleanup(func() { _ = unix.Close(a); _ = unix.Close(peer) })
	for _, fd := range []int{a, peer} {
		if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_SNDBUF, sndbuf885x42); err != nil {
			t.Fatalf("SO_SNDBUF: %v", err)
		}
	}
	chunk := make([]byte, 64<<10)
	filled := 0
	for {
		n, err := unix.Write(peer, chunk)
		if err != nil || n <= 0 {
			break
		}
		filled += n
	}
	const turn = driverReadBudget * driverReadBufSize
	if filled <= turn {
		t.Skipf("apparatus: the socketpair took %d bytes, not more than one turn (%d)", filled, turn)
	}

	total, reads := 0, 0
	last := make(chan [2]int, 1)
	if err := l.RegisterConn(a, func(p []byte) {
		total += len(p)
		reads++
		if total == filled {
			// On the loop thread, the loop's own state: the counter as of
			// the end of the previous round.
			last <- [2]int{l.adaptiveTimeoutMs(1), int(l.consecutiveEmpty)}
		}
	}, func(error) {}); err != nil {
		t.Fatalf("RegisterConn: %v", err)
	}
	t.Cleanup(func() { _ = l.UnregisterConn(a) })
	select {
	case got := <-last:
		turns := (filled + turn - 1) / turn
		t.Logf("celeris885 item 42: %d bytes (%d turns) read in %d reads; on the last, consecutiveEmpty=%d and the next idle wait would be %d ms",
			filled, turns, reads, got[1], got[0])
		if got[1] != 0 {
			t.Errorf("after %d turns of queued reads (epoll_wait(0) rounds that collected nothing) consecutiveEmpty=%d, want 0: a round the queue "+
				"forced to timeout 0 must not count as an idle round (celeris#885 item 42)", turns, got[1])
		}
		if turns > 11 && got[0] != 1 {
			t.Errorf("after %d turns the idle wait is %d ms, want the base, 1 ms (celeris#885 item 42)", turns, got[0])
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("the loop read %d of %d bytes in 10 s", total, filled)
	}
}
