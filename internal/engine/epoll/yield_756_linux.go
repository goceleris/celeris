//go:build linux

package epoll

import (
	"runtime"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/mono"
)

// yieldMinGap is the shortest time between two idle-wait yields of one loop
// (celeris#756). A yield hands the loop's P to the runtime for a thread
// handoff each way, which costs tens of microseconds; the gap bounds that
// cost for a loop that goes idle and busy again at a high rate (small
// messages), where the handlers it readied are served by the loop's own
// next turns anyway.
const yieldMinGap = 100 * time.Microsecond

// epollWaitYielding is unix.EpollWait with one addition: a loop that has
// readied a handler goroutine since its last wait and finds nothing else to
// do hands its P over before it blocks (celeris#756).
//
// The loop thread is locked to its OS thread and blocks in epoll_wait with
// its P. A handler goroutine it readied sits in that P's run queue and runs
// only when another P steals it, which needs a free P: while the GC's mark
// workers hold them, the handler (and the echo it owes the peer) waits for
// the next scheduler tick. runtime.Gosched on a locked goroutine runs the
// stoplockedm / handoffp path: another M takes the P and runs the handler at
// once, and the loop resumes when its goroutine is picked up again.
//
// The yield is taken only when the wait would block: a non-blocking
// epoll_wait found no event, so a loop that is busy (events ready, a loaded
// server) never pays the handoff, and a loop that readied nothing (HTTP/1
// sync) never pays the extra system call. At most one yield per yieldMinGap.
func (l *Loop) epollWaitYielding(timeoutMs int) (int, error) {
	if !l.yieldReady {
		return unix.EpollWait(l.epollFD, l.events, timeoutMs)
	}
	l.yieldReady = false
	if timeoutMs == 0 {
		return unix.EpollWait(l.epollFD, l.events, 0)
	}
	n, err := unix.EpollWait(l.epollFD, l.events, 0)
	if err != nil || n > 0 {
		return n, err
	}
	if now := mono.Now(); now-l.yieldLastNs >= int64(yieldMinGap) {
		l.yieldLastNs = now
		runtime.Gosched()
	}
	return unix.EpollWait(l.epollFD, l.events, timeoutMs)
}
