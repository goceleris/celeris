//go:build linux

package iouring

import (
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

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
// the sweep would wait for the clock to catch up. Worker thread; the caller
// reads the clock only when it has no batch clock to use.
func (w *Worker) sweepDue(now int64, gateHit bool) bool {
	d := now - w.lastSweep
	return d >= sweepCeiling || d < 0 || w.sweepTickFired || (gateHit && d >= sweepFloor)
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

// armSweepTick makes sure an IORING_OP_TIMEOUT of sweepCeiling is outstanding
// before the run loop waits for a SEND's completion with no timeout (mode 3a),
// and reports whether the wait may go ahead untimed (celeris#884 item 74).
// Mode 3a ends with the next CQE of any conn, and a SEND under longSendBytes
// into a socket that has no room is unmarked (markLongSend) and produces none,
// so a worker whose last submission is such a SEND never ran another
// iteration, and so no sweep, and WriteTimeout was never looked at. The timer
// is the kernel's guarantee of a CQE: its completion (handleSweepTick) ends the
// wait and has the next iteration sweep. It costs one SQE and one CQE per
// second, in the submit the wait makes anyway, and a bool test for every wait
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
	w.sweepTickSpec.Sec = sweepCeiling / int64(time.Second)
	w.sweepTickSpec.Nsec = sweepCeiling % int64(time.Second)
	prepTimeout(sqe, unsafe.Pointer(&w.sweepTickSpec), 0, 0)
	setSQEUserData(sqe, encodeUserData(udSweepTick, 0))
	w.sweepTickArmed = true
	return true
}

// handleSweepTick takes the standing sweep timer's CQE: the timer is no longer
// outstanding (the next mode-3a wait arms another) and, when it ran its
// second (-ETIME), the loop owes a sweep (sweepDue). Any other result (-EINVAL
// from a kernel that refuses the SQE, -ECANCELED, -ENOMEM) means the timer did
// not do its job: it is no reason to sweep, and arming another would end every
// mode-3a wait at once and make a full sweep, past the floor, after every
// request. The worker then stops arming it (sweepTickBroken, logged once) and
// the loop takes the timed wait, as it does with no SQE to be had. Worker
// thread.
func (w *Worker) handleSweepTick(res int32) {
	w.sweepTickArmed = false
	if res == -int32(unix.ETIME) {
		w.sweepTickFired = true
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
