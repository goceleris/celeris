//go:build linux

package iouring

import (
	"testing"
	"time"
	"unsafe"
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
