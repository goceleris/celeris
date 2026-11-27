//go:build linux

package epoll

import (
	"math"
	"time"
)

// The walk of the live connections, made when something is due (celeris#1430).
//
// checkTimeouts walks every connection the loop holds: 42 to 71 ns each, cache
// cold, which at 10,000 idle keep-alive connections was 40 walks a second and
// 16 % of a CPU at 50,000. Nothing is due for most of them for most of the time:
// the walk now ends by saying when the next one is, and the loop makes it then.
//
// What stays the same is the wake-up. The sweep timer still ticks every
// sweepFloor and the wait tiers are untouched, so the loop wakes as often as
// before (that is celeris#1430 part W, a longer wait, which needs a wake for each
// deadline that is set off the loop). What is skipped is the call to
// checkTimeouts at a tick, or at the gate, when sweepDue says no: the walk of the
// connections, not the tick.
//
// The next walk is due at the earliest of:
//
//   - the deadline a connection can be shown to have: the accept budget of a
//     connection whose protocol is not decided, an armed header deadline, and
//     last read + min(IdleTimeout, ReadTimeout). Each is exact, and is met at
//     the first tick at or after it, the tick a walk at every tick would have
//     used.
//   - the next tick, for what the walk cannot date: a sampled clock (the
//     closing drain, the write-rate window of a parked response), a connection
//     the walk could not read (its lock is held), one it acted on, an HTTP/2
//     connection that is exempt, a hijack in flight. The walk looks at these at
//     every tick, as before.
//   - sweepHorizon, the safety cap: at most a second after this walk, less when
//     a timeout is short (see below).
//
// And a loop is not skipped at all while it holds an HTTP/2 connection or a
// detached one (their deadlines are set and moved off the loop, by the stream
// layer and by the middleware's goroutine, with no event for the loop to lower
// anything at), while it holds no connection (nothing to walk), and while it has
// a connection on its dirty list.
//
// Why a clock that starts between two walks needs no hook of its own, except the
// two below: a deadline that a connection gets from an event at time t is at
// least t plus a timeout of the configuration (the accept budget, the header
// deadline armed by the data that arrived, the idle and read deadline from the
// read), and the walk before t was at most sweepHorizon ago, which is half the
// shortest of those timeouts (or a second). The next walk therefore comes before
// the deadline, finds it, and dates it exactly. The clocks that are not of that
// shape are the two the hooks start (a parked response's write clock, a closing
// drain's clock: lowerNextDue at armEpollOut, handleWritable and markClosing), and
// the ones set off the loop, which pin the loop. The structural test of this
// change (sweep_due_structure_1430_linux_test.go) lists every clock the sweep
// reads and how each is accounted for, and fails for one that is not.

// sweepHorizonMax is the longest the loop goes without a walk of its live
// connections: the safety cap. A deadline the next-due computation does not know
// is met up to this late (one second, where a walk at every tick met it within
// sweepFloor), not for ever.
const sweepHorizonMax = time.Second

// sweepDue reports whether the loop's tick or gate should walk the live
// connections now. Loop thread.
func (l *Loop) sweepDue(now int64) bool {
	// A clock that stepped back: the next-due time was set against the old
	// one, and the walk must not wait for the new one to catch up.
	return now >= l.nextDue || now < l.lastSweep ||
		len(l.liveConns) == 0 || len(l.h2Conns) != 0 || l.detachedCount != 0 || l.dirtyHead != nil
}

// lowerNextDue brings the next walk forward to t when t is earlier than the
// time it is due. Loop thread.
func (l *Loop) lowerNextDue(t int64) {
	if t < l.nextDue {
		l.nextDue = t
	}
}

// sweepHorizon is how long after a walk the next one is due when no connection
// has an earlier deadline: sweepHorizonMax, or half the shortest timeout that a
// new deadline can be made of (ReadHeaderTimeout, ReadTimeout, IdleTimeout) when
// that is shorter, and never less than sweepFloor, the rate the walk had.
func (l *Loop) sweepHorizon() int64 {
	h := int64(sweepHorizonMax)
	fresh := time.Duration(math.MaxInt64)
	for _, d := range [...]time.Duration{l.cfg.ReadHeaderTimeout, l.cfg.ReadTimeout, l.cfg.IdleTimeout} {
		if d > 0 && d < fresh {
			fresh = d
		}
	}
	if half := int64(fresh / 2); half < h {
		h = max(half, int64(sweepFloor))
	}
	return h
}

// idleBound is min(IdleTimeout, ReadTimeout), the time a connection may go
// without a read, as timeoutsDue tests it; a large value, which a deadline
// computed from it never reaches, when neither is set.
func (l *Loop) idleBound() int64 {
	b := int64(math.MaxInt64 / 4)
	for _, d := range [...]time.Duration{l.cfg.IdleTimeout, l.cfg.ReadTimeout} {
		if d > 0 && int64(d) < b {
			b = int64(d)
		}
	}
	return b
}
