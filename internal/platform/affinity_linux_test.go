//go:build linux

package platform

import (
	"runtime"
	"testing"

	"golang.org/x/sys/unix"
)

// TestSaveThreadAffinityRestoresThePin: Restore gives the thread back exactly
// the mask SaveThreadAffinity saw before PinToCPU narrowed it to one CPU. The
// engine loops rely on it for the one thread the runtime cannot terminate,
// the main thread (celeris#905).
func TestSaveThreadAffinityRestoresThePin(t *testing.T) {
	// Never unlocked: the test goroutine exits locked, so the runtime ends
	// its thread even if a Restore below failed and left it pinned.
	runtime.LockOSThread()

	var before unix.CPUSet
	if err := unix.SchedGetaffinity(0, &before); err != nil {
		t.Fatalf("sched_getaffinity: %v", err)
	}
	if before.Count() < 2 {
		t.Skipf("the thread may run on %d CPU only: a pin to one CPU changes nothing to restore", before.Count())
	}
	// Pin to the highest CPU in the mask, so the pinned mask differs from
	// the original whichever CPUs the process was given.
	cpu := -1
	for i := range len(before) * 64 {
		if before.IsSet(i) {
			cpu = i
		}
	}

	prev, err := SaveThreadAffinity()
	if err != nil {
		t.Fatalf("SaveThreadAffinity: %v", err)
	}
	if err := PinToCPU(cpu); err != nil {
		t.Fatalf("PinToCPU(%d): %v", cpu, err)
	}
	var pinned unix.CPUSet
	if err := unix.SchedGetaffinity(0, &pinned); err != nil {
		t.Fatalf("sched_getaffinity: %v", err)
	}
	if pinned.Count() != 1 || !pinned.IsSet(cpu) {
		_ = prev.Restore()
		t.Fatalf("premise: after PinToCPU(%d) the thread may run on %d CPU(s), want only CPU %d",
			cpu, pinned.Count(), cpu)
	}

	if err := prev.Restore(); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	var after unix.CPUSet
	if err := unix.SchedGetaffinity(0, &after); err != nil {
		t.Fatalf("sched_getaffinity: %v", err)
	}
	if after != before {
		t.Fatalf("after Restore the thread may run on %d CPU(s), want the %d it had before the pin",
			after.Count(), before.Count())
	}
}
