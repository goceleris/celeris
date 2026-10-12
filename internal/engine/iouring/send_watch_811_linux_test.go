//go:build linux

package iouring

import (
	"testing"
	"time"
)

// TestIouringDirtyPassGiveUpSetsTheSendWatch811: the give-up leaves a parked
// worker with no spin to reach its timeout sweep, so it sets sendWatch; the
// loop then sweeps about every 1 to 2 s (checked on every 8th iteration),
// and a sweep that finds no conn with a SEND outstanding ends the watch.
func TestIouringDirtyPassGiveUpSetsTheSendWatch811(t *testing.T) {
	const tail = "the rest of response 1"
	rig := newStallRig704(t)
	w, cs := rig.w, rig.cs
	w.cfg.WriteTimeout = time.Hour
	w.cfg.IdleTimeout = time.Hour
	w.cfg.ReadTimeout = time.Hour
	inFlight750(rig, tail, false)
	takeSQEs(w.ring)
	if w.sendWatch {
		t.Fatal("apparatus: the watch is set before any give-up")
	}
	w.markDirty(cs)
	w.flushDirty()
	if !w.sendWatch {
		t.Fatalf("celeris#811: the dirty pass gave up a conn with a SEND outstanding and did not set sendWatch: " +
			"a parked worker would sweep for its WriteTimeout only at the idle gate, up to ~100 s apart")
	}

	for _, tc := range []struct {
		name     string
		tick     uint32
		sweptAgo time.Duration
		want     bool
	}{
		{"due", 8, 2 * time.Second, true},
		{"swept_just_now", 8, 10 * time.Millisecond, false},
		{"not_an_8th_iteration", 9, 2 * time.Second, false},
	} {
		w.tickCounter = tc.tick
		w.lastSweep = time.Now().Add(-tc.sweptAgo).UnixNano()
		if got := w.sweepWatchDue(); got != tc.want {
			t.Errorf("sweepWatchDue %s (tick %d, last sweep %v ago) = %v, want %v", tc.name, tc.tick, tc.sweptAgo, got, tc.want)
		}
	}

	w.checkTimeouts() // the SEND is still outstanding: the watch stays
	if !w.sendWatch || w.lastSweep == 0 {
		t.Errorf("a sweep that finds a conn with a SEND outstanding: sendWatch=%v lastSweep=%d, want true and set", w.sendWatch, w.lastSweep)
	}
	w.tickCounter = 8
	if w.sweepWatchDue() {
		t.Errorf("sweepWatchDue right after a sweep")
	}

	w.handleSend(sendCQE750(rig, int32(len(tail)), 0), rig.local, time.Now().UnixNano())
	if cs.sending {
		t.Fatalf("apparatus: the SEND is still outstanding after its completion")
	}
	w.checkTimeouts()
	if w.sendWatch {
		t.Errorf("a sweep that finds no conn with a SEND outstanding left sendWatch set: every sweep would stay on the 1 s cadence")
	}
}
