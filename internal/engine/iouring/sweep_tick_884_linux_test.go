//go:build linux

package iouring

import (
	"runtime"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// celeris#884 items 65, 74 and 75 at the unit level: the sweep's gate (sweepDue)
// and the standing timer that ends an untimed SEND wait (armSweepTick).

func TestSweepDue884(t *testing.T) {
	const ms = int64(time.Millisecond)
	base := int64(1_000_000_000_000)
	for _, tc := range []struct {
		name    string
		since   int64 // now - lastSweep
		gateHit bool
		fired   bool
		want    bool
	}{
		{"busy gate hit inside the floor", 24 * ms, true, false, false},
		{"busy gate hit at the floor", 25 * ms, true, false, true},
		{"no gate hit between floor and ceiling", 999 * ms, false, false, false},
		{"no gate hit at the ceiling", 1000 * ms, false, false, true},
		{"idle past the ceiling", 5000 * ms, false, false, true},
		{"the clock stepped back", -3 * ms, false, false, true},
		{"the clock stepped back, gate hit", -3 * ms, true, false, true},
		{"the standing timer fired, nothing else due", 1 * ms, false, true, true},
		{"the same sweep, just made", 0, true, false, false},
	} {
		w := &Worker{lastSweep: base, sweepTickFired: tc.fired}
		if got := w.sweepDue(base+tc.since, tc.gateHit); got != tc.want {
			t.Errorf("celeris884 %s: sweepDue(%d ms after the last sweep, gateHit=%v, fired=%v) = %v, want %v",
				tc.name, tc.since/ms, tc.gateHit, tc.fired, got, tc.want)
		}
	}
}

// TestArmSweepTickPlacesOneStandingTimer884: the timer is one IORING_OP_TIMEOUT
// of sweepCeiling, tagged udSweepTick; arming it again while it is outstanding
// places nothing; its CQE (through the cancel path's processCQE, which has its
// own case) clears the outstanding flag and owes a sweep, the sweep clears
// that, and the next wait arms another. With no SQE to be had it reports false
// and the loop waits with a timeout.
func TestArmSweepTickPlacesOneStandingTimer884(t *testing.T) {
	f := newFDLFixture(t, false)
	w := f.w
	_ = takeSQEs(w.ring)

	if !w.armSweepTick() {
		t.Fatal("celeris884: armSweepTick reported no timer with an empty SQ ring")
	}
	sqes := takeSQEs(w.ring)
	if len(sqes) != 1 || sqes[0].op != opTIMEOUT || sqes[0].ud != encodeUserData(udSweepTick, 0) {
		t.Fatalf("celeris884: arming placed %v, want one TIMEOUT tagged udSweepTick", sqes)
	}
	if sqes[0].addr != uint64(uintptr(unsafe.Pointer(&w.sweepTickSpec))) {
		t.Error("celeris884: the timer's timespec is not the worker's own, which the kernel reads at the submit")
	}
	if w.sweepTickSpec.Sec != 1 || w.sweepTickSpec.Nsec != 0 {
		t.Errorf("celeris884: the timer is %d s %d ns, want sweepCeiling (1 s)", w.sweepTickSpec.Sec, w.sweepTickSpec.Nsec)
	}
	if !w.armSweepTick() || len(takeSQEs(w.ring)) != 0 {
		t.Fatal("celeris884: arming while the timer is outstanding must place nothing and say the wait may go ahead")
	}

	f.process(&completionEntry{UserData: encodeUserData(udSweepTick, 0), Res: -62})
	if w.sweepTickArmed || !w.sweepTickFired {
		t.Fatalf("celeris884: after the timer's CQE armed=%v fired=%v, want false and true", w.sweepTickArmed, w.sweepTickFired)
	}
	if !w.sweepDue(w.lastSweep, false) {
		t.Error("celeris884: a fired timer must make the next iteration sweep")
	}
	w.checkTimeouts()
	if w.sweepTickFired {
		t.Error("celeris884: the sweep left the fired flag set")
	}
	if !w.armSweepTick() || len(takeSQEs(w.ring)) != 1 {
		t.Error("celeris884: the next wait must arm another timer")
	}
	f.process(&completionEntry{UserData: encodeUserData(udSweepTick, 0), Res: -62})

	for w.ring.GetSQE() != nil { // fill the SQ ring
	}
	if w.armSweepTick() {
		t.Error("celeris884: armSweepTick reported a timer with the SQ ring full: the loop would wait with no timeout")
	}
	if w.sweepTickArmed {
		t.Error("celeris884: a timer that was not placed is recorded as outstanding")
	}
	_ = takeSQEs(w.ring)
}

// TestSweepTickAnswerOtherThanETIMEDoesNotForceSweeps884: a timer that the
// kernel completes with anything but -ETIME (-EINVAL on a kernel or tier that
// refuses the SQE, -ECANCELED, -ENOMEM) did not run for its second, so it is
// no reason to sweep, and arming another would end every mode-3a wait at once
// and make a full sweep, past the floor, after every request. The worker must
// stop arming it, so the loop takes the timed wait.
func TestSweepTickAnswerOtherThanETIMEDoesNotForceSweeps884(t *testing.T) {
	for _, res := range []int32{-int32(unix.EINVAL), -int32(unix.ECANCELED), -int32(unix.ENOMEM), 0} {
		f := newFDLFixture(t, false)
		w := f.w
		_ = takeSQEs(w.ring)
		if !w.armSweepTick() {
			t.Fatal("celeris884: armSweepTick reported no timer with an empty SQ ring")
		}
		_ = takeSQEs(w.ring)
		f.process(&completionEntry{UserData: encodeUserData(udSweepTick, 0), Res: res})
		if w.sweepTickFired {
			t.Errorf("celeris884: a tick completed with res=%d, not -ETIME, owes a sweep past the floor", res)
		}
		if w.armSweepTick() {
			t.Errorf("celeris884: after a tick completed with res=%d the worker arms another, so every mode-3a wait "+
				"would end at once", res)
		}
		if n := len(takeSQEs(w.ring)); n != 0 {
			t.Errorf("celeris884: after a tick completed with res=%d the worker placed %d more SQE(s)", res, n)
		}
	}
}

// TestSweepTickOnARealRing884: the kernel accepts the tick's SQE under the
// setup flags the engine uses, and answers it with -ETIME about
// sweepCeiling after the submit (the unit test above feeds a made-up CQE).
func TestSweepTickOnARealRing884(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	rings := 0
	for _, fl := range []struct {
		name  string
		flags uint32
	}{
		{"plain", 0},
		{"defer_taskrun", setupDeferTaskrun | setupSingleIssuer},
		{"coop_taskrun", setupCoopTaskrun | setupSingleIssuer},
	} {
		r, err := NewRing(64, fl.flags, 0)
		if err != nil {
			t.Logf("celeris884 TICK flags=%s: ring unavailable: %v", fl.name, err)
			continue
		}
		rings++
		w := &Worker{ring: r}
		if !w.armSweepTick() {
			t.Fatalf("flags=%s: no SQE for the tick", fl.name)
		}
		t0 := time.Now()
		h, tl, _, err := waitCQE1183(r)
		if err != nil {
			t.Fatalf("flags=%s: wait: %v", fl.name, err)
		}
		el := time.Since(t0)
		if h == tl {
			t.Fatalf("flags=%s: the wait returned with no CQE", fl.name)
		}
		c := r.cqeAt(h)
		ud, res := c.UserData, c.Res
		r.EndCQ(h + 1)
		_ = r.Close()
		t.Logf("celeris884 TICK flags=%s tag_ok=%v res=%d elapsed_ms=%d", fl.name, ud == encodeUserData(udSweepTick, 0), res, el.Milliseconds())
		if ud != encodeUserData(udSweepTick, 0) {
			t.Errorf("flags=%s: the CQE is %#x, not the tick's", fl.name, ud)
		}
		if res != -int32(unix.ETIME) {
			t.Errorf("flags=%s: the tick completed with res=%d, want -ETIME (%d)", fl.name, res, -int32(unix.ETIME))
		}
		if el < 900*time.Millisecond || el > 3*time.Second {
			t.Errorf("flags=%s: the tick came %v after the submit, want about 1 s", fl.name, el)
		}
	}
	if rings == 0 {
		skipOrFail656(t, "no ring could be set up with any of the engine's flag sets")
	}
}

// waitCQE1183 waits on r until its CQ ring has a completion, or 10 s have gone
// by, and returns the CQ ring's head and tail and how many times a wait came
// back with the CQ ring empty. The condition is a completion, not one wait
// returning: SubmitAndWait returns nil on a signal, and a wait can end with
// nothing to read (celeris#1183). Each retry is a new io_uring_enter with
// GETEVENTS, as the run loop's iterations are: under DEFER_TASKRUN a
// completion is posted by that call and a look at the CQ ring would not
// find it.
func waitCQE1183(r *Ring) (head, tail uint32, empty int, err error) {
	for deadline := time.Now().Add(10 * time.Second); ; {
		if err = r.SubmitAndWait(); err != nil {
			return 0, 0, empty, err
		}
		if head, tail = r.BeginCQ(); head != tail {
			return head, tail, empty, nil
		}
		empty++
		if !time.Now().Before(deadline) {
			return head, tail, empty, nil
		}
	}
}

// TestSweepTickWaitSurvivesAnEmptyReturn1183: Ring.SubmitAndWait returns nil
// when a signal ends the wait (EINTR, as its comment says, and the engine's
// loop relies on), so a nil return is not a completion; a wait with min_complete
// 1 can also come back at once with nothing to read (measured on a loaded
// 7.0 kernel, with no signal: 5 of 4000 fresh coop_taskrun rings). The run loop
// iterates on an empty return; the test of the tick on a real ring read the CQ
// ring after a single wait and failed "the wait returned with no CQE"
// (celeris#1183). Here Go's preemption signal (SIGURG, which the runtime ignores
// when it has not asked for it) ends the wait of a 300 ms timer every 5 ms, and
// the wait must still deliver the timer's CQE.
func TestSweepTickWaitSurvivesAnEmptyReturn1183(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	pid, tid := unix.Getpid(), unix.Gettid()
	rings := 0
	for _, fl := range []struct {
		name  string
		flags uint32
	}{
		{"plain", 0},
		{"defer_taskrun", setupDeferTaskrun | setupSingleIssuer},
		{"coop_taskrun", setupCoopTaskrun | setupSingleIssuer},
	} {
		r, err := NewRing(64, fl.flags, 0)
		if err != nil {
			t.Logf("celeris1183 flags=%s: ring unavailable: %v", fl.name, err)
			continue
		}
		rings++
		spec := kernelTimespec{Nsec: int64(300 * time.Millisecond)}
		prepTimeout(r.GetSQE(), unsafe.Pointer(&spec), 0, 0)
		stop, done := make(chan struct{}), make(chan struct{})
		go func() {
			defer close(done)
			for tk := time.NewTicker(5 * time.Millisecond); ; {
				select {
				case <-stop:
					tk.Stop()
					return
				case <-tk.C:
					_ = unix.Tgkill(pid, tid, unix.SIGURG)
				}
			}
		}()
		h, tl, empty, err := waitCQE1183(r)
		close(stop)
		<-done
		runtime.KeepAlive(&spec)
		if err != nil {
			_ = r.Close()
			t.Fatalf("flags=%s: wait: %v", fl.name, err)
		}
		if h == tl {
			_ = r.Close()
			t.Fatalf("flags=%s: the wait returned with no CQE (%d empty returns)", fl.name, empty)
		}
		res := r.cqeAt(h).Res
		r.EndCQ(h + 1)
		_ = r.Close()
		t.Logf("celeris1183 flags=%s empty_returns=%d res=%d", fl.name, empty, res)
		if res != -int32(unix.ETIME) {
			t.Errorf("flags=%s: the timer completed with res=%d, want -ETIME", fl.name, res)
		}
		if empty == 0 {
			t.Errorf("flags=%s: no signal ended the wait early: the test is not exercising an empty return", fl.name)
		}
	}
	if rings == 0 {
		skipOrFail656(t, "no ring could be set up with any of the engine's flag sets")
	}
}

// TestSweepClock884: an empty iteration may be a zero-wait spin, so it reads the
// clock only where the read can matter; every other iteration passes the batch's
// clock (a sentinel here, which no clock read can equal).
func TestSweepClock884(t *testing.T) {
	const stale = int64(12345)
	for _, tc := range []struct {
		name    string
		gotCQE  bool
		gateHit bool
		fired   bool
		empty   int
		dirty   bool
		tick    uint32
		fresh   bool
		noLn    bool // the worker has no listener: baseTimeout is 1 s
	}{
		{"an iteration with CQEs uses the batch clock", true, true, true, 100, false, 0, false, false},
		{"an empty spin with the dirty list non-empty", false, false, false, 100, true, 1, false, false},
		{"an empty 1 ms wait, between reads", false, false, false, 3, false, 1, false, false},
		{"an empty wait of 5 ms or more", false, false, false, 100, false, 1, true, false},
		{"an empty spin on every eighth iteration", false, false, false, 100, true, 8, true, false},
		{"an empty first iteration on a worker with no listener (1 s waits)", false, false, false, 3, false, 1, true, true},
		{"an empty iteration with a dirty conn on a worker with no listener", false, false, false, 100, true, 1, true, true},
		{"an empty iteration at a gate hit", false, true, false, 3, true, 1, true, false},
		{"an empty iteration after the standing timer fired", false, false, true, 3, true, 1, true, false},
	} {
		w := &Worker{cachedNow: stale, emptyIters: uint32(tc.empty), tickCounter: tc.tick, sweepTickFired: tc.fired}
		if tc.noLn {
			w.listenFD = -1
		}
		if tc.dirty {
			w.dirtyHead = &connState{}
		}
		got := w.sweepClock(tc.gotCQE, tc.gateHit)
		if fresh := got != stale; fresh != tc.fresh {
			t.Errorf("celeris884 %s: sweepClock read a fresh clock = %v, want %v", tc.name, fresh, tc.fresh)
		}
	}
}

// TestSweepTickBrokenStopsArming884 is the latch's other half: once broken, no
// timer is placed and the wait may not go untimed.
func TestSweepTickBrokenStopsArming884(t *testing.T) {
	f := newFDLFixture(t, false)
	w := f.w
	_ = takeSQEs(w.ring)
	w.sweepTickBroken = true
	if w.armSweepTick() || len(takeSQEs(w.ring)) != 0 {
		t.Error("celeris884: a worker whose timer failed must place none and report that the wait may not go untimed")
	}
}
