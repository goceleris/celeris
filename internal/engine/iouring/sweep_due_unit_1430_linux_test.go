//go:build linux

package iouring

import (
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/mono"
)

// celeris#1430 at the unit level: the next look (nextDue) and what reads it. The
// kernel is out of the loop, so every time is the test's own. These cannot fail
// on the base first: they name fields and functions that the change adds.

const ms1430 = int64(time.Millisecond)

// TestSweepDueWithANextLook1430: the rows of TestSweepDue884 with a next look.
// The gate, the ceiling, the step back and the standing timer are what they were;
// a next look adds a sweep at its time, and never sooner than the floor after the
// last sweep.
func TestSweepDueWithANextLook1430(t *testing.T) {
	base := int64(1_000_000_000_000)
	for _, tc := range []struct {
		name    string
		nextDue int64 // relative to lastSweep; 0: none known
		since   int64 // now - lastSweep
		gateHit bool
		want    bool
	}{
		{"none known, between floor and ceiling", 0, 300 * ms1430, false, false},
		{"due in 300 ms, 299 ms after", 300 * ms1430, 299 * ms1430, false, false},
		{"due in 300 ms, at it", 300 * ms1430, 300 * ms1430, false, true},
		{"due in 300 ms, after it", 300 * ms1430, 450 * ms1430, false, true},
		{"due in 5 ms, inside the floor", 5 * ms1430, 10 * ms1430, false, false},
		{"due in 5 ms, at the floor", 5 * ms1430, 25 * ms1430, false, true},
		{"due in the past, at the floor", -50 * ms1430, 25 * ms1430, false, true},
		{"due in the past, inside the floor", -50 * ms1430, 24 * ms1430, false, false},
		{"due at the ceiling", 1000 * ms1430, 999 * ms1430, false, false},
		{"the clock stepped back with a look pending", 300 * ms1430, -3 * ms1430, false, true},
		{"a gate hit inside the floor with a look pending", 300 * ms1430, 24 * ms1430, true, false},
	} {
		w := &Worker{lastSweep: base}
		if tc.nextDue != 0 {
			w.nextDue = base + tc.nextDue
		}
		if got := w.sweepDue(base+tc.since, tc.gateHit); got != tc.want {
			t.Errorf("celeris1430 %s: sweepDue(%d ms after the last sweep) = %v, want %v", tc.name, tc.since/ms1430, got, tc.want)
		}
	}
}

// TestDueAtKeepsTheEarliestLookAheadOfNow1430: dueAt lowers the next look to the
// earliest time that is ahead of now; a time at or before now is not a look (the
// sweep either closed the conn or leaves it to the sweeps' period), and 0 means
// none known.
func TestDueAtKeepsTheEarliestLookAheadOfNow1430(t *testing.T) {
	const now = int64(1_000_000_000_000)
	w := &Worker{}
	w.dueAt(now, now)
	w.dueAt(now-1, now)
	w.dueAt(0, now)
	if w.nextDue != 0 {
		t.Fatalf("celeris1430: times at or before now set nextDue = %d, want none", w.nextDue)
	}
	w.dueAt(now+500*ms1430, now)
	w.dueAt(now+900*ms1430, now)
	w.dueAt(now+200*ms1430, now)
	w.dueAt(now-10*ms1430, now)
	if want := now + 200*ms1430; w.nextDue != want {
		t.Errorf("celeris1430: nextDue = %d, want the earliest ahead of now, %d", w.nextDue, want)
	}
}

// TestNextLookFloorAndCeiling1430: the floor is applied where the look is read, so
// an arm site that lowers it inside the floor is not a sweep every iteration; the
// ceiling where the sweep ends, so a stale look is at most a second early.
func TestNextLookFloorAndCeiling1430(t *testing.T) {
	const now = int64(1_000_000_000_000)
	w := &Worker{lastSweep: now}
	if got := w.dueTime(); got != 0 {
		t.Errorf("celeris1430: dueTime with none known = %d, want 0", got)
	}
	w.dueAt(now+5*ms1430, now)
	if got, want := w.dueTime(), now+sweepFloor; got != want {
		t.Errorf("celeris1430: dueTime of a look 5 ms after the sweep = %d (%+d ms), want the floor, %+d ms", got, (got-now)/ms1430, sweepFloor/ms1430)
	}
	w.nextDue = now + 300*ms1430
	if got := w.dueTime(); got != now+300*ms1430 {
		t.Errorf("celeris1430: dueTime of a look 300 ms after the sweep = %+d ms, want 300", (got-now)/ms1430)
	}

	for _, tc := range []struct {
		name    string
		nextDue int64
		want    int64
	}{
		{"no deadline known leaves the ceiling", 0, now + sweepCeiling},
		{"a look within the ceiling stays", now + 300*ms1430, now + 300*ms1430},
		{"a look beyond the ceiling is brought to it", now + 60*int64(time.Second), now + sweepCeiling},
	} {
		w := &Worker{nextDue: tc.nextDue}
		w.finishDue(now)
		if w.nextDue != tc.want {
			t.Errorf("celeris1430 %s: nextDue after the sweep = %+d ms, want %+d ms", tc.name, (w.nextDue-now)/ms1430, (tc.want-now)/ms1430)
		}
	}
}

// TestSweptTimeoutIsCappedAtTheNextLook1430: the ring wait ends at the next look,
// whichever wait it is: the 100 ms of an idle worker, the 25 ms of one with
// ReadHeaderTimeout, and the second of a worker with no listener (which
// sweptTimeout also returns from its transplant cap).
func TestSweptTimeoutIsCappedAtTheNextLook1430(t *testing.T) {
	for _, tc := range []struct {
		name      string
		noListen  bool
		rht       time.Duration
		look      time.Duration // from now; 0: none known
		want      time.Duration // an upper bound
		wantAtMin time.Duration // and a lower one
	}{
		{"an idle worker, look in 30 ms", false, -1, 30 * time.Millisecond, 30 * time.Millisecond, 5 * time.Millisecond},
		{"an idle worker, look in 3 s: the wait is its own", false, -1, 3 * time.Second, 100 * time.Millisecond, 100 * time.Millisecond},
		{"an idle worker, no look known", false, -1, 0, 100 * time.Millisecond, 100 * time.Millisecond},
		{"ReadHeaderTimeout on: its 25 ms wait is not lengthened by a later look", false, 10 * time.Second, 200 * time.Millisecond, 25 * time.Millisecond, 25 * time.Millisecond},
		{"no listener, look in 300 ms", true, -1, 300 * time.Millisecond, 300 * time.Millisecond, 100 * time.Millisecond},
		{"no listener, no look known", true, -1, 0, time.Second, time.Second},
	} {
		w := &Worker{emptyIters: 200}
		w.cfg.ReadHeaderTimeout = tc.rht
		if tc.noListen {
			w.listenFD = -1
		}
		if tc.look != 0 {
			w.lastSweep = mono.Now()
			w.nextDue = w.lastSweep + int64(tc.look)
		}
		got := w.sweptTimeout()
		if got > tc.want || got < tc.wantAtMin {
			t.Errorf("celeris1430 %s: sweptTimeout = %v, want between %v and %v", tc.name, got, tc.wantAtMin, tc.want)
		}
	}
}

// TestSweepTickFollowsTheNextLook1430: the standing timer that ends an untimed
// SEND wait is armed for the next look when that is sooner than a second, and an
// outstanding timer that would fire after a look that was lowered since is not
// waited on untimed (sweepTickOK).
func TestSweepTickFollowsTheNextLook1430(t *testing.T) {
	f := newFDLFixture(t, false)
	w := f.w
	_ = takeSQEs(w.ring)
	now := mono.Now()
	w.lastSweep = now
	w.nextDue = now + 200*ms1430
	if !w.sweepTickOK() {
		t.Fatal("celeris1430: sweepTickOK reported no timer with an empty SQ ring")
	}
	if sqes := takeSQEs(w.ring); len(sqes) != 1 || sqes[0].op != opTIMEOUT {
		t.Fatalf("celeris1430: arming placed %v, want one TIMEOUT", sqes)
	}
	spec := time.Duration(w.sweepTickSpec.Sec)*time.Second + time.Duration(w.sweepTickSpec.Nsec)
	if spec <= 100*time.Millisecond || spec > 200*time.Millisecond {
		t.Errorf("celeris1430: the timer is %v, want the time left to the look (about 200 ms)", spec)
	}
	if !w.sweepTickOK() || len(takeSQEs(w.ring)) != 0 {
		t.Error("celeris1430: an outstanding timer that fires at the look must be waited on untimed, and not armed again")
	}
	// A look lowered after the timer was armed: the timer is later than it.
	w.nextDue = now + 50*ms1430
	w.dueAt(now+50*ms1430, now)
	if w.sweepTickOK() {
		t.Error("celeris1430: an outstanding timer later than the next look was waited on untimed: the wait would end after the look")
	}
	// The timer that is outstanding and no later than the look is fine.
	w.nextDue = now + 5*int64(time.Second)
	if !w.sweepTickOK() {
		t.Error("celeris1430: an outstanding timer sooner than the next look must still end the untimed wait")
	}
}

// TestArmSiteHelpers1430: the lowers the arm sites make, with the clocks they are
// given.
func TestArmSiteHelpers1430(t *testing.T) {
	const base = int64(1_000_000_000_000)
	w := &Worker{cachedNow: base}
	w.cfg.ReadTimeout = 300 * time.Millisecond
	w.cfg.IdleTimeout = 200 * time.Millisecond
	w.cfg.WriteTimeout = 500 * time.Millisecond
	w.dueIdleFrom(base, base)
	if want := base + 200*ms1430 + 1; w.nextDue != want {
		t.Errorf("celeris1430: dueIdleFrom with IdleTimeout 200 ms and ReadTimeout 300 ms = %+d ms, want 200 ms and 1 ns", (w.nextDue-base)/ms1430)
	}
	w.nextDue = 0
	w.cfg.IdleTimeout = 0
	w.dueIdleFrom(base, base)
	if want := base + 300*ms1430 + 1; w.nextDue != want {
		t.Errorf("celeris1430: dueIdleFrom with IdleTimeout off = %+d ms, want ReadTimeout", (w.nextDue-base)/ms1430)
	}
	w.nextDue = 0
	cs := &connState{lastActivity: base - 100*ms1430}
	w.dueWrite(cs)
	if want := base + 400*ms1430 + 1; w.nextDue != want {
		t.Errorf("celeris1430: dueWrite = %+d ms, want the write clock's end, 400 ms", (w.nextDue-base)/ms1430)
	}
	w.nextDue = 0
	w.cfg.WriteTimeout = 0
	w.dueWrite(cs)
	if w.nextDue != 0 {
		t.Errorf("celeris1430: dueWrite with WriteTimeout off set a look")
	}
}

// TestCheckTimeoutsComputesTheNextLook1430: for each state a connection can be in,
// the next look the sweep leaves. The clocks are absolute, so the expected value
// is exact.
func TestCheckTimeoutsComputesTheNextLook1430(t *testing.T) {
	type rowT struct {
		name string
		cfg  func(w *Worker)
		set  func(f *fdlFixture, base int64)
		// want is the next look relative to base; 0: none (the ceiling).
		want time.Duration
	}
	rows := []rowT{
		{"idle: ReadTimeout and IdleTimeout", func(w *Worker) {
			w.cfg.ReadTimeout, w.cfg.IdleTimeout = 300*time.Millisecond, time.Second
		}, func(f *fdlFixture, base int64) { f.cs.lastActivity = base }, 300*time.Millisecond + 1},
		{"idle: IdleTimeout alone", func(w *Worker) { w.cfg.IdleTimeout = 200 * time.Millisecond },
			func(f *fdlFixture, base int64) { f.cs.lastActivity = base }, 200*time.Millisecond + 1},
		{"idle: no timeout configured leaves the ceiling", func(w *Worker) {},
			func(f *fdlFixture, base int64) { f.cs.lastActivity = base }, 0},
		{"idle: a timeout past the ceiling leaves the ceiling", func(w *Worker) { w.cfg.ReadTimeout = time.Minute },
			func(f *fdlFixture, base int64) { f.cs.lastActivity = base }, 0},
		{"a send in flight: the write clock", func(w *Worker) { w.cfg.WriteTimeout = 250 * time.Millisecond },
			func(f *fdlFixture, base int64) { f.cs.lastActivity, f.cs.sending = base, true }, 250*time.Millisecond + 1},
		{"a send in flight with WriteTimeout off: the idle clock for when it completes", func(w *Worker) { w.cfg.IdleTimeout = 400 * time.Millisecond },
			func(f *fdlFixture, base int64) { f.cs.lastActivity, f.cs.sending = base, true }, 400*time.Millisecond + 1},
		{"a header deadline", func(w *Worker) {},
			func(f *fdlFixture, base int64) {
				f.cs.h1State.HeaderDeadlineNs.Store(base + 150*int64(time.Millisecond))
			}, 150*time.Millisecond + 1},
		{"a protocol not decided: its budget", func(w *Worker) {},
			func(f *fdlFixture, base int64) {
				f.cs.detected, f.cs.detectDeadline = false, base+120*int64(time.Millisecond)
			}, 120*time.Millisecond + 1},
		{"a detached conn: the middleware's idle deadline", func(w *Worker) { w.cfg.IdleTimeout = 100 * time.Millisecond },
			func(f *fdlFixture, base int64) {
				f.cs.h1State.Detached.Store(true)
				f.cs.h1State.IdleDeadlineNs.Store(base + 180*int64(time.Millisecond))
			}, 180*time.Millisecond + 1},
		{"a closing conn: the next sample of the peer's progress", func(w *Worker) {},
			func(f *fdlFixture, base int64) {
				f.cs.closing = true
				f.cs.lastActivity = base
				f.cs.closeSampled = base - closingDrainTimeoutNanos/4 + 50*int64(time.Millisecond)
			}, 50 * time.Millisecond},
		{"a closing conn: the reap, when the sample is not due first", func(w *Worker) { w.cfg.WriteTimeout = 800 * time.Millisecond },
			func(f *fdlFixture, base int64) {
				f.cs.closing = true
				f.cs.lastActivity = base - closingDrainTimeoutNanos + 300*int64(time.Millisecond)
				f.cs.closeSampled = base
				f.cs.fixedFile = true // no sample for a fixed-file conn
			}, 300*time.Millisecond + 1},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			f := newFDLFixture(t, false)
			f.w.cfg.ReadHeaderTimeout = 0
			f.w.cfg.ReadTimeout, f.w.cfg.IdleTimeout, f.w.cfg.WriteTimeout = 0, 0, 0
			row.cfg(f.w)
			base := mono.Now()
			row.set(f, base)
			f.w.checkTimeouts()
			if f.w.conns[f.fd] != f.cs {
				t.Fatal("celeris1430 PREMISE: the sweep closed the conn")
			}
			want := base + int64(row.want)
			if row.want == 0 {
				// The ceiling, from the sweep's own clock.
				if got := f.w.nextDue - f.w.lastSweep; got != sweepCeiling {
					t.Errorf("celeris1430: nextDue is %+d ms after the sweep, want the ceiling (%d ms)", got/ms1430, sweepCeiling/ms1430)
				}
				return
			}
			if f.w.nextDue != want {
				t.Errorf("celeris1430: nextDue = %+d ns from the state's clock, want %+d ns", f.w.nextDue-base, want-base)
			}
		})
	}
}

// TestCheckTimeoutsLeavesWhatItCannotSeeToTheSweepsPeriod1430: a conn inside an
// async handler (the snapshot's lock is held) and one past a timeout that the
// sweep does not close contribute no look; the sweeps that were made before are
// what meets them.
func TestCheckTimeoutsLeavesWhatItCannotSeeToTheSweepsPeriod1430(t *testing.T) {
	f := newFDLFixture(t, true)
	f.w.cfg.ReadTimeout = 300 * time.Millisecond
	f.cs.lastActivity = mono.Now()
	if f.cs.detachMu == nil {
		t.Fatal("celeris1430 PREMISE: the async fixture conn has no detachMu")
	}
	f.cs.detachMu.Lock()
	f.w.checkTimeouts()
	f.cs.detachMu.Unlock()
	if got := f.w.nextDue - f.w.lastSweep; got != sweepCeiling {
		t.Errorf("celeris1430: a conn inside its handler left a look %+d ms after the sweep, want the ceiling (none known)", got/ms1430)
	}
	// With the lock free the same conn contributes.
	f.w.checkTimeouts()
	if got := f.w.nextDue - f.w.lastSweep; got >= sweepCeiling {
		t.Errorf("celeris1430: with the lock free the look is %+d ms after the sweep, want ReadTimeout's, under the ceiling", got/ms1430)
	}
}

// TestSweepTickInsideTheFloorOwesNoSweep1430: the standing timer is armed for the
// next look, and a sweep that another iteration made within the floor before the
// time it was aimed at is not made again because the timer fired.
func TestSweepTickInsideTheFloorOwesNoSweep1430(t *testing.T) {
	const base = int64(1_000_000_000_000)
	for _, tc := range []struct {
		name  string
		aimed int64 // relative to the last sweep
		res   int32
		want  bool
	}{
		{"aimed 10 ms after a sweep", 10 * ms1430, -int32(unix.ETIME), false},
		{"aimed at the floor after a sweep", sweepFloor, -int32(unix.ETIME), true},
		{"aimed a second after a sweep", 1000 * ms1430, -int32(unix.ETIME), true},
		{"aimed before the last sweep", -5 * ms1430, -int32(unix.ETIME), false},
		{"another result owes none", 1000 * ms1430, -int32(unix.ECANCELED), false},
	} {
		w := &Worker{lastSweep: base, sweepTickAt: base + tc.aimed, sweepTickArmed: true}
		w.handleSweepTick(tc.res)
		if w.sweepTickArmed || w.sweepTickFired != tc.want {
			t.Errorf("celeris1430 %s: after the timer's CQE armed=%v fired=%v, want false and %v", tc.name, w.sweepTickArmed, w.sweepTickFired, tc.want)
		}
	}
}
