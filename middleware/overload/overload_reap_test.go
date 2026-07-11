package overload

import (
	"runtime"
	"testing"
	"time"
)

// TestReapGCFiresOnceOnEntryNotPerTick is a regression guard for celeris#407:
// the opt-in Reap GC must fire on the transition INTO Reap, not on every poll
// tick the stage lingers in the Reap band. Pre-fix this forced a GC (STW) every
// PollInterval under sustained moderate load.
func TestReapGCFiresOnceOnEntryNotPerTick(t *testing.T) {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	before := ms.NumForcedGC

	m := &mockCPU{}
	m.set(0.82) // in the Reap band [Reap=0.80, Reorder=0.85)
	poll := 5 * time.Millisecond
	_, ctrl := NewWithController(Config{
		CollectorProvider:  withMock(m),
		PollInterval:       poll,
		EnableReap:         true,
		ReapAggressiveness: 1,
	})
	defer ctrl.Stop()

	waitForStage(t, ctrl, StageReap)
	// waitForStage returns the instant stage.Store publishes StageReap, but the
	// entry runtime.GC() runs after that in the same tick — wait a few ticks so
	// it (and only it) has completed before sampling.
	time.Sleep(3 * poll)

	// Entering Reap must fire the opt-in GC exactly once.
	runtime.ReadMemStats(&ms)
	atEntry := ms.NumForcedGC
	if atEntry <= before {
		t.Fatalf("entering Reap did not fire the opt-in GC (NumForcedGC %d -> %d)", before, atEntry)
	}

	// Linger in the Reap band for ~30 poll intervals. Pre-fix (celeris#407)
	// forces a GC every tick (~30 more); post-fix fires 0 more.
	time.Sleep(30 * poll)
	if ctrl.Stage() != StageReap {
		t.Fatalf("expected to stay in Reap, got %s", ctrl.Stage())
	}
	runtime.ReadMemStats(&ms)
	if lingering := ms.NumForcedGC - atEntry; lingering > 1 {
		t.Fatalf("Reap GC fired %d extra times while lingering in the band; want <=1 (celeris#407: GC only on entry)", lingering)
	}
}
