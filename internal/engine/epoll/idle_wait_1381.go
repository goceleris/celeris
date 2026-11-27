//go:build linux

package epoll

import (
	"context"
	"time"

	"golang.org/x/sys/unix"
)

// The long wait of a loop that has nothing to serve (celeris#1381).
//
// A loop with no connection of any kind and nothing queued has nothing to
// time out and nobody to answer. It used to wake every 4 ms for it (the wait
// of its idlest tier) and again every 25 ms (the sweep timer, through the same
// epoll set): about 285 wake-ups a second per loop, whatever the traffic,
// and the wake-ups kept the Go runtime's sysmon thread polling every 20 us. Such
// a loop now waits idleWaitMs and ticks its sweep timer every idleWaitMs, about
// 10 wake-ups a second.
//
// What ends the long wait is what ends any wait: an epoll event (an accept, a
// connection's bytes, a driver conn's event) or a write of the wake eventfd,
// which every producer of work for the loop makes: adopt, detach, ask, h2
// write queue, ResumeAccept, StartTransplant, and, since this change,
// BeginPauseAccept and the cancel of the loop's context (AfterFunc in run).
// Both of those used to ride the 4 ms wait without a wake of their own. The
// wait stays bounded (never infinite), so a producer that is added without a
// wake costs one idleWaitMs, not a hang.
//
// The first event restores the 25 ms timer: consecutiveEmpty goes to 0 with it,
// the predicate is false at the top of the next iteration, and the timer is
// re-armed before the loop waits again. With any connection the loop waits and
// ticks exactly as before: the predicate is false, and so is timerSlow. (What
// the tick does there changed in celeris#1430: the walk of the connections is
// made when something is due, see sweep_due_1430.go; the wake-up is the same.)

// idleWaitMs is the wait, and the period of the sweep timer, of a loop that
// holds nothing and has been idle for idleEmptyRounds rounds.
const idleWaitMs = 100

// idleEmptyRounds is the number of consecutive empty rounds after which an
// idle loop may take the long wait: the same 100 that open the 4 ms tier of
// adaptiveTimeoutMs.
const idleEmptyRounds = 100

// eventfdCreate is unix.Eventfd; a test replaces it to fail the creation of a
// loop's wake eventfd. Set before an engine is created and restored after it
// stops.
var eventfdCreate = unix.Eventfd

// idleQuiescent reports whether the loop holds nothing and has nothing queued:
// no connection of any kind, no work owed to it, no drain, pause or linger in
// progress, and the descriptors that wake it. Loop thread only. The facts are
// the ones the DRAINING to SUSPENDED park tests, plus the open listener, the
// timer and the eventfd, and what the post-switch sweep and the batched
// counters have left to publish.
//
// What counts as live: connCount and liveConns hold every HTTP conn the loop
// owns, HTTP/2 and detached (WebSocket, SSE) ones included (they leave only at
// closeConn); a hijacked conn and one handed to the other engine have left
// them, and what is still owed for either is in detachQPending and
// transplantInFlight. Driver conns are in no count: hasDriverConns.
func (l *Loop) idleQuiescent(ctx context.Context) bool {
	return !l.idleWaitOff.Load() && l.listenFD >= 0 && l.timerFD >= 0 && l.wakeFD.FD() >= 0 && !l.listenHot &&
		l.connCount == 0 && len(l.liveConns) == 0 && len(l.h2Conns) == 0 && l.detachedCount == 0 &&
		l.dirtyHead == nil && len(l.driverReadQ) == 0 && l.lingerUntil == 0 &&
		l.transplant.Load() == nil && l.sweepTS == nil && l.sweepPub == ([numResidual]uint64{}) &&
		l.transplantInFlight == 0 && l.h2DrainStart.IsZero() &&
		!l.hasDriverConns.Load() && l.detachQPending.Load() == 0 && l.adoptQPending.Load() == 0 &&
		l.xferAskPending.Load() == 0 && !l.acceptPaused.Load() &&
		l.reqBatch == 0 && l.bytesReadBatch == 0 && l.bytesWrittenBatch == 0 &&
		ctx.Err() == nil
}

// timerfdSettime is unix.TimerfdSettime; a test replaces it to fail the re-arm.
var timerfdSettime = unix.TimerfdSettime

// setSweepPeriod arms the sweep timerfd at the idle period (slow) or at
// sweepFloor, and records which once the timer is armed. The loop thread, at
// the top of an iteration, before it waits. A re-arm that fails is not recorded,
// so the next iteration tries again (the loop meanwhile waits on its timeout
// alone, which is bounded the same, and a loop that holds a connection is not
// left believing its timer is at sweepFloor). A loop whose timer a test has
// disarmed by hand is left as it is (idleWaitOff).
func (l *Loop) setSweepPeriod(slow bool) {
	if !slow && l.idleWaitOff.Load() {
		l.timerSlow = false
		return
	}
	p := int64(sweepFloor)
	if slow {
		p = idleWaitMs * int64(time.Millisecond)
	}
	spec := unix.ItimerSpec{
		Interval: unix.Timespec{Sec: p / int64(time.Second), Nsec: p % int64(time.Second)},
		Value:    unix.Timespec{Sec: p / int64(time.Second), Nsec: p % int64(time.Second)},
	}
	if timerfdSettime(l.timerFD, 0, &spec, nil) == nil {
		l.timerSlow = slow
	}
}
