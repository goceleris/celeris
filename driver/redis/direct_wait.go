package redis

import (
	"context"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// How execManyDirect waits for the first bytes of a pipeline's reply
// (celeris#854). A direct-mode conn can park the goroutine in Go's netpoll
// (cheap when the reply is quick) or wait in poll(2) on the caller's own
// thread (cheaper when the round trip is long: no netpoll wake-up hop). The
// switch below picks one for the whole process; the counters say what each
// Exec did. Both are unexported on purpose: no public API, and the tests and a
// probe built into the package read them.

// directWaitKind is the wait-mode switch.
type directWaitKind int32

const (
	// directWaitUnset means the environment has not been read yet.
	directWaitUnset directWaitKind = -1
	// directWaitNetpoll always parks in Go's netpoll (the behaviour before
	// #854). It is the default.
	directWaitNetpoll directWaitKind = 0
	// directWaitAuto lets each conn choose from its own first-chunk wait.
	directWaitAuto directWaitKind = 1
	// directWaitBlock always waits in poll(2) (where the platform has it).
	directWaitBlock directWaitKind = 2
)

// directWaitEnv names the environment variable that sets the mode: "netpoll"
// (the default), "auto" or "block", compared after trimming spaces and folding
// case. Any other value means netpoll: a mistyped setting never switches the
// feature on. It is read on the first Exec that needs it, not at program start,
// so a program may os.Setenv it first.
const directWaitEnv = "CELERIS_REDIS_DIRECT_WAIT"

// directWaitMode is the process-wide mode, directWaitUnset until first use;
// tests set it through setDirectWaitForTest. directWaitOnce guards the read.
var (
	directWaitMode atomic.Int32
	directWaitOnce sync.Once
)

// init only stores the "not read yet" sentinel; the environment is not read here.
func init() { directWaitMode.Store(int32(directWaitUnset)) }

// currentDirectWait returns the mode, reading the environment on first use.
func currentDirectWait() directWaitKind {
	if m := directWaitKind(directWaitMode.Load()); m != directWaitUnset {
		return m
	}
	directWaitOnce.Do(func() {
		directWaitMode.CompareAndSwap(int32(directWaitUnset), int32(parseDirectWait(os.Getenv(directWaitEnv))))
	})
	if m := directWaitKind(directWaitMode.Load()); m != directWaitUnset {
		return m
	}
	return directWaitNetpoll // a test reset the mode between the two loads
}

// parseDirectWait maps the environment value to a mode; unknown means netpoll.
func parseDirectWait(s string) directWaitKind {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "auto":
		return directWaitAuto
	case "block":
		return directWaitBlock
	default:
		return directWaitNetpoll
	}
}

// paddedCounter keeps one counter on a cache line of its own: the counters are
// process-wide and written from every Exec, so sharing a line would make the
// Execs of different conns contend on each other's counters.
type paddedCounter struct {
	_ [64]byte
	atomic.Uint64
	_ [56]byte
}

// directWaitCounters count what the direct pipeline path did, process-wide.
type directWaitCounters struct {
	// blocks counts waits started in poll(2): one per episode of waiting for
	// bytes, not one per poll call.
	blocks paddedCounter
	// sliceExpiries counts poll(2) slices that ended with nothing to read and
	// were polled again. Informational: it says how long the thread waited.
	sliceExpiries paddedCounter
	// handoffs counts the waits that used up directPollBudget in poll(2) and
	// were handed to Go's netpoll (tcp.Read) with the reply still not there.
	handoffs paddedCounter
	// netpollParks counts the reads handed to Go's netpoll (tcp.Read) after the
	// non-blocking peek found nothing, handoffs included, by waits in auto or
	// block mode. The default netpoll mode runs the pre-#854 loop and counts
	// nothing: blocks and handoffs staying 0 is how a test sees it did not block.
	netpollParks paddedCounter
}

// directWaitStats is the process-wide instance.
var directWaitStats directWaitCounters

// directWaitSnap is a copy of the counters.
type directWaitSnap struct{ Blocks, SliceExpiries, Handoffs, NetpollParks uint64 }

// directWaitSnapshot reads the counters.
func directWaitSnapshot() directWaitSnap {
	return directWaitSnap{
		Blocks:        directWaitStats.blocks.Load(),
		SliceExpiries: directWaitStats.sliceExpiries.Load(),
		Handoffs:      directWaitStats.handoffs.Load(),
		NetpollParks:  directWaitStats.netpollParks.Load(),
	}
}

// since returns the counters' growth since the earlier snapshot s.
func (s directWaitSnap) since(e directWaitSnap) directWaitSnap {
	return directWaitSnap{s.Blocks - e.Blocks, s.SliceExpiries - e.SliceExpiries, s.Handoffs - e.Handoffs, s.NetpollParks - e.NetpollParks}
}

// The auto mode's switch, per conn, on the moving average of the first-chunk
// wait: the time from the pipeline's write returning to the first reply bytes
// in hand. It is measured the same way whichever wait the Exec used, so the
// choice does not feed back into the signal beyond the ~100 us a blocking wait
// saves, which is less than the gap between the two thresholds.
const (
	// directBlockOnAbove switches the blocking wait on.
	directBlockOnAbove = 300 * time.Microsecond
	// directBlockOffBelow switches it off again.
	directBlockOffBelow = 150 * time.Microsecond
)

// directBlockNext is the hysteresis: a conn that is not blocking starts above
// directBlockOnAbove, one that is stops below directBlockOffBelow.
func directBlockNext(on bool, ewmaNs int64) bool {
	if on {
		return ewmaNs >= int64(directBlockOffBelow)
	}
	return ewmaNs > int64(directBlockOnAbove)
}

// observeDirectWait folds one first-chunk wait into c's average (weight 1/4,
// so a change of regime shows within a few Execs) and re-decides the wait.
// Called under c.writerMu.
func (c *redisConn) observeDirectWait(d time.Duration) {
	if c.directWaitEWMA == 0 {
		c.directWaitEWMA = int64(d)
	} else {
		c.directWaitEWMA += (int64(d) - c.directWaitEWMA) / 4
	}
	c.directBlockOn = directBlockNext(c.directBlockOn, c.directWaitEWMA)
}

// awaitDirectReplies is the reply loop of execManyDirect in auto and block mode
// (celeris#854), entered after the pipeline was written, with the conn's
// writerMu held and the socket deadline set. It is the pre-#854 loop plus the
// poll(2) wait after a peek that found nothing, and, in auto mode only, the
// timing of the first-chunk wait. A nil return means last has finished.
func (c *redisConn) awaitDirectReplies(ctx context.Context, mode directWaitKind, last *redisRequest) error {
	block := c.tcpFd > 0 && (mode == directWaitBlock || (mode == directWaitAuto && c.directBlockOn))
	dl, hasDl := ctx.Deadline()
	// The first-chunk wait runs from here to the first reply bytes in hand,
	// whichever way they came.
	var t0 time.Time
	timing := mode == directWaitAuto
	if timing {
		t0 = time.Now()
	}
	for !last.finished.Load() {
		if c.tcpFd > 0 {
			if rn, _, perr := syscall.Recvfrom(c.tcpFd, c.syncBuf, msgDontWait); rn > 0 {
				if timing {
					c.observeDirectWait(time.Since(t0))
					timing = false
				}
				c.onRecv(c.syncBuf[:rn])
				continue
			} else if perr != nil && perr != syscall.EAGAIN && perr != syscall.EWOULDBLOCK {
				_ = c.closeWithErr(perr)
				return perr
			} else if block && perr != nil && directPollIn(ctx, c.tcpFd, dl, hasDl) {
				continue
			}
		}
		directWaitStats.netpollParks.Add(1)
		rn, err := c.tcp.Read(c.syncBuf)
		if rn > 0 {
			if timing {
				c.observeDirectWait(time.Since(t0))
				timing = false
			}
			c.onRecv(c.syncBuf[:rn])
		}
		if err != nil {
			_ = c.closeWithErr(err)
			return err
		}
	}
	return nil
}
