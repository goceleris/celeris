//go:build linux

package iouring

import (
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/conn"
	"github.com/goceleris/celeris/internal/mono"
)

// The timeout sweep's pacing (celeris#884 items 65, 74 and 75): the time gate
// Worker.run asks (sweepDue) and the standing timer that ends an untimed SEND
// wait (armSweepTick). Worker.run keeps the gate's call, the mode-3a condition
// and the two CQE dispatch cases; checkTimeouts clears sweepTickFired.

// sweepFloor is the least time between two sweeps that the run loop's
// iteration gate asks for, and sweepCeiling the most that may pass before any
// iteration sweeps (celeris#884 items 75 and 65, sweepDue). The floor is
// epoll's (celeris#885 item 29): a busy worker's iterations are microseconds
// apart, and a gate that counted them alone swept thousands of times a second.
// The ceiling is the bound sweepWatchDue already gave a worker with a SEND
// outstanding, and what the standing sweep timer (armSweepTick) holds an
// untimed SEND wait to. An idle worker with ReadHeaderTimeout off and no
// detached conn, whose gate was 1024 iterations of 100 ms (about 92 to 102 s),
// now sweeps about once a second (up to about two on a worker with no
// listener, whose idle wait is a second: the ceiling is seen at the end of an
// iteration). Both are nanoseconds, as lastSweep is.
const (
	sweepFloor   = int64(25 * time.Millisecond)
	sweepCeiling = int64(time.Second)
)

// sweepDue reports whether the loop owes a timeout sweep at now (UnixNano),
// given whether the iteration gate hit (celeris#884 items 75 and 65): a gate hit
// sweeps when sweepFloor has passed since the last sweep, whoever made it, and
// any iteration sweeps once sweepCeiling has, so an idle worker whose gate is
// 1024 iterations of 100 ms still sweeps about every second. A sweep the
// worker was told to make by its standing timer (sweepTickFired) is made
// whatever the clock says; so is one after the clock stepped back (d < 0), or
// the sweep would wait for the clock to catch up. A sweep is also made when the
// last one found a deadline falling due now (dueNow, celeris#1430): that is
// added to the sweeps above, none of which is switched off. It counts as a sweep
// for the floor and the ceiling like any other, so a gate hit within the floor
// after it does not sweep. Worker thread; the caller reads the clock only when it
// has no batch clock to use.
func (w *Worker) sweepDue(now int64, gateHit bool) bool {
	d := now - w.lastSweep
	return d >= sweepCeiling || d < 0 || w.sweepTickFired || (gateHit && d >= sweepFloor) || w.dueNow(now)
}

// The next look (celeris#1430). Each timeout sweep works out the earliest time at
// which a connection's deadline could fall due (nextDue), and the loop sweeps
// then, as well as at the sweeps it made before: the gate, the ceiling, the
// standing timer. None of those is switched off, so a deadline the computation
// does not know (a connection inside an async handler, one exempt from a timeout,
// an HTTP/2 deadline lowered from a pool goroutine, a WebSocket deadline moved by
// the middleware) is met by them, within the old period of its due time: a
// sweep added for a look counts as a sweep for the floor and the ceiling, so a
// gate hit within the floor after it is skipped and the ceiling counts from it.
// Three rules bound what the computation costs:
//
//   - nextDue is never sooner than sweepFloor after the last sweep (dueTime), so
//     the sweeps the computation adds are at most the 40 a second of the floor;
//   - it is never more than sweepCeiling after the sweep that computed it
//     (finishDue), so a stale value is at most a second of an early look;
//   - a deadline that is already past after a sweep contributes nothing (dueAt):
//     the sweep either closed the connection or left it to the sweeps' period (a
//     connection exempt from a timeout, or one whose lock was held). A sweep that
//     wants the next look soon says so with now plus the floor.
//
// The sites that arm a deadline, and the sweeps' own reads of the clocks, are
// listed by a test (sweep_due_sites_1430_linux_test.go) that parses them. Every
// one runs on the worker's thread, which is what nextDue is owned by.

// dueAt lowers the next look to t, when t is ahead of now. 0 is "none known".
func (w *Worker) dueAt(t, now int64) {
	if t > now && (w.nextDue == 0 || t < w.nextDue) {
		w.nextDue = t
	}
}

// dueTime is when the next look is owed, no sooner than the floor after the
// last sweep, or 0 when none is known.
func (w *Worker) dueTime() int64 {
	if w.nextDue == 0 {
		return 0
	}
	return max(w.nextDue, w.lastSweep+sweepFloor)
}

// dueNow reports whether the next look is owed at now.
func (w *Worker) dueNow(now int64) bool {
	t := w.dueTime()
	return t != 0 && now >= t
}

// dueWait is the time left to the next look, never below zero, and whether one is
// known. It reads the clock: it is for the waits, not for the sweep's loop.
func (w *Worker) dueWait() (time.Duration, bool) {
	t := w.dueTime()
	if t == 0 {
		return 0, false
	}
	return time.Duration(max(t-mono.Now(), 0)), true
}

// dueCap caps a ring wait at the time left to the next look.
func (w *Worker) dueCap(d time.Duration) time.Duration {
	if left, ok := w.dueWait(); ok && left < d {
		return left
	}
	return d
}

// finishDue ends a sweep's computation: no sooner than the floor is applied
// where nextDue is read (dueTime); no later than the ceiling is applied here,
// and a sweep that found no deadline to wait for leaves the ceiling.
func (w *Worker) finishDue(now int64) {
	if w.nextDue == 0 || w.nextDue > now+sweepCeiling {
		w.nextDue = now + sweepCeiling
	}
}

// dueIdleFrom lowers the next look to when a connection whose idle clock (the
// last read or completed send) stood at base would pass ReadTimeout or
// IdleTimeout. A conn whose clock is later than that by the time it is looked at
// costs one early look, which computes the time again.
func (w *Worker) dueIdleFrom(base, now int64) {
	if rt := w.cfg.ReadTimeout; rt > 0 {
		w.dueAt(base+int64(rt)+1, now)
	}
	if it := w.cfg.IdleTimeout; it > 0 {
		w.dueAt(base+int64(it)+1, now)
	}
}

// dueWrite lowers the next look to when the write clock of a connection with
// output in flight (its last read or completed send plus WriteTimeout) runs out.
func (w *Worker) dueWrite(cs *connState) {
	if wt := w.cfg.WriteTimeout; wt > 0 {
		w.dueAt(cs.lastActivity+int64(wt)+1, w.cachedNow)
	}
}

// dueH2 lowers the next look to the earliest deadline of an HTTP/2 connection
// (conn.H2State.NextDeadline). A deadline that is owed already (a batch of
// expired requests that the sweep's budget cut off, a sweep whose lock was held)
// is looked at one floor from now.
func (w *Worker) dueH2(h *conn.H2State, now int64) {
	if h == nil {
		return
	}
	if t := h.NextDeadline(now); t != 0 {
		if t <= now {
			t = now + sweepFloor
		}
		w.dueAt(t, now)
	}
}

// sweepClock is the clock sweepDue is given. (Since celeris#1173 the loop reads
// the clock after every wait, readClockAfterWait, so cachedNow is never older
// than the iteration and the fresh reads below are second ones on the empty
// iterations; they stay because the cadence tests and the benchmark pin this
// function, and can go in a follow-up.)
// sweepClock is the clock sweepDue is given: the batch's own (cachedNow) on an
// iteration that had CQEs, and on an empty one a fresh read only where it can
// matter, because an empty iteration may be a zero-wait spin (baseTimeout is 0
// while the dirty list holds a conn, 1 ms for the first ten empty iterations,
// 100 microseconds with H2 conns and no wake descriptor) and a read costs about
// 35 ns. A fresh read is taken at a gate hit, when the standing timer fired,
// whenever the wait is long (hasLongIdleWait: a worker with no listener waits
// a second whatever its empty-iteration count, and a stale clock would hold its
// sweeps up to eight seconds apart), after ten empty iterations with the dirty
// list empty (a wait of 5 ms or more, or 100 microseconds with H2 conns and no
// wake descriptor), and on every eighth iteration otherwise; the stale clock is never
// ahead of lastSweep (checkTimeouts sets both), so it never makes a sweep due
// by itself, it only defers the ceiling's by the iterations it covers. Worker
// thread.
func (w *Worker) sweepClock(gotCQE, gateHit bool) int64 {
	if gotCQE {
		return w.cachedNow
	}
	if gateHit || w.sweepTickFired || w.hasLongIdleWait() || (w.emptyIters > 10 && w.dirtyHead == nil) || w.tickCounter&0x7 == 0 {
		return mono.Now()
	}
	return w.cachedNow
}

// hasLongIdleWait reports whether an empty iteration of this worker waits a
// second on the ring, whatever its empty-iteration count or dirty list: a
// worker with no listener (paused, standby, stopping). baseTimeout answers its
// first case with it, so the two cannot diverge (celeris#884 items 65 and 75,
// review r2 of #1114). Worker thread.
func (w *Worker) hasLongIdleWait() bool { return w.listenFD < 0 }

// armSweepTick makes sure an IORING_OP_TIMEOUT is outstanding before the run
// loop waits for a SEND's completion with no timeout (mode 3a), and reports
// whether the wait may go ahead untimed (celeris#884 item 74). The timer is
// sweepCeiling long, or less when the next look is sooner (dueWait,
// celeris#1430). Mode 3a ends with the next CQE of any conn, and a SEND under
// longSendBytes into a socket that has no room is unmarked (markLongSend) and
// produces none, so a worker whose last submission is such a SEND never ran
// another iteration, and so no sweep, and WriteTimeout was never looked at. The
// timer is the kernel's guarantee of a CQE: its completion (handleSweepTick)
// ends the wait and has the next iteration sweep. It costs one SQE and one CQE
// per second, in the submit the wait makes anyway, and a bool test for every wait
// after that: not the ext_arg a timed wait pays on each one, which is what mode
// 3a exists to skip. With no SQE to be had it reports false and the loop waits
// with a timeout instead. Worker thread.
func (w *Worker) armSweepTick() bool {
	if w.sweepTickArmed {
		return true
	}
	if w.sweepTickBroken {
		return false
	}
	sqe := w.ring.GetSQE()
	if sqe == nil {
		return false
	}
	now := mono.Now()
	d := sweepCeiling
	if t := w.dueTime(); t != 0 && t-now < d {
		d = max(t-now, int64(time.Millisecond))
	}
	w.sweepTickSpec.Sec = d / int64(time.Second)
	w.sweepTickSpec.Nsec = d % int64(time.Second)
	prepTimeout(sqe, unsafe.Pointer(&w.sweepTickSpec), 0, 0)
	setSQEUserData(sqe, encodeUserData(udSweepTick, 0))
	w.sweepTickArmed = true
	w.sweepTickAt = now + d
	return true
}

// sweepTickOK reports whether the loop may wait for a SEND with no timeout: the
// standing timer is outstanding, or has been armed now, and fires no later than
// the next look. A timer armed for a later time than a look that has been
// lowered since (an accept, a send started) would hold the wait past it, so that
// wait is the timed one, capped at the look (celeris#1430). Worker thread.
func (w *Worker) sweepTickOK() bool {
	if w.sweepTickArmed {
		t := w.dueTime()
		return t == 0 || w.sweepTickAt <= t+int64(time.Millisecond)
	}
	return w.armSweepTick()
}

// handleSweepTick takes the standing sweep timer's CQE: the timer is no longer
// outstanding (the next mode-3a wait arms another) and, when it ran its
// time (-ETIME), the loop owes a sweep (sweepDue), unless one ran within the
// floor before it. Any other result (-EINVAL
// from a kernel that refuses the SQE, -ECANCELED, -ENOMEM) means the timer did
// not do its job: it is no reason to sweep, and arming another would end every
// mode-3a wait at once and make a full sweep, past the floor, after every
// request. The worker then stops arming it (sweepTickBroken, logged once) and
// the loop takes the timed wait, as it does with no SQE to be had. Worker
// thread.
func (w *Worker) handleSweepTick(res int32) {
	w.sweepTickArmed = false
	if res == -int32(unix.ETIME) {
		// Not when a sweep ran within the floor before the time the timer was
		// aimed at: the timer is armed for the next look (celeris#1430), and
		// another iteration may have made that sweep first; a second one inside
		// the floor would be a sweep the floor forbids.
		if w.sweepTickAt-w.lastSweep >= sweepFloor {
			w.sweepTickFired = true
		}
		return
	}
	if !w.sweepTickBroken {
		w.sweepTickBroken = true
		if w.logger != nil {
			w.logger.Warn("io_uring: the sweep timer completed with an error; the worker waits for SEND completions with a timeout instead",
				"worker", w.id, "res", res)
		}
	}
}
