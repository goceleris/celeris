//go:build linux

package platform

import (
	"errors"
	"runtime"
	"slices"
	"testing"
	"unsafe"

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

// fakeKernel stands in for a kernel with possible CPUs, the number of CPUs
// nr_cpu_ids counts. Like Linux it answers sched_getaffinity with EINVAL to a
// buffer smaller than its mask (cpumask_size, in whole words), writes the
// mask, and takes any length in sched_setaffinity (a shorter one is zero
// extended, a longer one cut), refusing a mask with no CPU that exists. It
// holds one thread's mask, which starts as the CPUs in initial.
type fakeKernel struct {
	possible int
	mask     []byte
	getSizes []uintptr // the buffer size of every sched_getaffinity call
	setSizes []uintptr // the length of every sched_setaffinity call
}

func (k *fakeKernel) maskBytes() int { return (k.possible + 63) / 64 * 8 }

func (k *fakeKernel) has(cpu int) bool { return k.mask[cpu/8]&(1<<(cpu%8)) != 0 }

// installFakeKernel puts a fake kernel under the two affinity syscalls for the
// rest of the test.
func installFakeKernel(t *testing.T, possible int, initial ...int) *fakeKernel {
	t.Helper()
	k := &fakeKernel{possible: possible}
	k.mask = make([]byte, k.maskBytes())
	for _, c := range initial {
		k.mask[c/8] |= 1 << (c % 8)
	}
	prevGet, prevSet := sysSchedGetaffinity, sysSchedSetaffinity
	t.Cleanup(func() { sysSchedGetaffinity, sysSchedSetaffinity = prevGet, prevSet })
	sysSchedGetaffinity = func(_ int, size uintptr, ptr unsafe.Pointer) error {
		k.getSizes = append(k.getSizes, size)
		if size < uintptr(k.maskBytes()) {
			return unix.EINVAL
		}
		copy(unsafe.Slice((*byte)(ptr), size), k.mask)
		return nil
	}
	sysSchedSetaffinity = func(_ int, size uintptr, ptr unsafe.Pointer) error {
		k.setSizes = append(k.setSizes, size)
		in := unsafe.Slice((*byte)(ptr), min(int(size), k.maskBytes()))
		next := make([]byte, k.maskBytes())
		copy(next, in)
		found := false
		for cpu := range k.possible {
			found = found || next[cpu/8]&(1<<(cpu%8)) != 0
		}
		if !found {
			return unix.EINVAL
		}
		k.mask = next
		return nil
	}
	return k
}

// TestSaveThreadAffinityOnAKernelWithMoreThan1024CPUs (the checklist item
// "when platform.SaveThreadAffinity fails, the loop still calls PinToCPU" that
// #907's round-2 reviews added to celeris#884 and celeris#885): sched_getaffinity into a 1024-bit unix.CPUSet fails
// with EINVAL on a kernel with 2048 possible CPUs, and the engine loop that
// then skipped the save parked its main thread pinned. The save has to
// succeed, and Restore has to hand back every CPU of the saved mask, one the
// short buffer could not hold (CPU 1500) included.
func TestSaveThreadAffinityOnAKernelWithMoreThan1024CPUs(t *testing.T) {
	k := installFakeKernel(t, 2048, 0, 1, 2, 3, 1500)
	want := slices.Clone(k.mask)

	prev, err := SaveThreadAffinity()
	if err != nil {
		t.Fatalf("SaveThreadAffinity on a 2048-CPU kernel: %v (get buffer sizes tried: %v)", err, k.getSizes)
	}
	// The loop pins its thread, then restores on the way out.
	if err := PinToCPU(2); err != nil {
		t.Fatalf("PinToCPU(2): %v", err)
	}
	if !k.has(2) || k.has(1500) || k.has(0) {
		t.Fatalf("premise: PinToCPU(2) left the mask %v, want CPU 2 alone", k.cpus())
	}
	if err := prev.Restore(); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if !slices.Equal(k.mask, want) {
		t.Fatalf("after Restore the mask is %v (set lengths %v), want the saved %v: CPU 1500 is %v",
			k.cpus(), k.setSizes, cpusOf(want), k.has(1500))
	}
}

// A kernel with 1024 CPUs or fewer keeps working with the same calls, and a
// kernel with a mask bigger than the 8 KiB the save grows to is an error, not
// a hang or a panic.
func TestSaveThreadAffinityKernelSizes(t *testing.T) {
	for _, possible := range []int{8, 64, 1024} {
		k := installFakeKernel(t, possible, 0, possible-1)
		prev, err := SaveThreadAffinity()
		if err != nil {
			t.Fatalf("%d-CPU kernel: SaveThreadAffinity: %v", possible, err)
		}
		if err := PinToCPU(0); err != nil {
			t.Fatalf("%d-CPU kernel: PinToCPU(0): %v", possible, err)
		}
		if err := prev.Restore(); err != nil {
			t.Fatalf("%d-CPU kernel: Restore: %v", possible, err)
		}
		if !k.has(0) || !k.has(possible-1) {
			t.Errorf("%d-CPU kernel: Restore left the mask %v", possible, k.cpus())
		}
	}
	installFakeKernel(t, 100000, 0)
	if _, err := SaveThreadAffinity(); !errors.Is(err, unix.EINVAL) {
		t.Errorf("a 100000-CPU kernel: SaveThreadAffinity = %v, want EINVAL", err)
	}
}

// schedAllowedCPUs has the same 1024-bit read: on a 2048-CPU kernel it failed,
// and PlanWorkerCPUs fell back to the CPU numbers 0..NumCPU-1 for a process
// whose mask may name none of them.
func TestAllowedCPUsOnAKernelWithMoreThan1024CPUs(t *testing.T) {
	installFakeKernel(t, 2048, 3, 1500, 2047)
	got, err := schedAllowedCPUs()
	if err != nil {
		t.Fatalf("schedAllowedCPUs on a 2048-CPU kernel: %v", err)
	}
	eq(t, "allowed", got, []int{3, 1500, 2047})
	if p := PlanWorkerCPUs(3, false); !slices.Equal(p.Allowed, []int{3, 1500, 2047}) {
		t.Errorf("PlanWorkerCPUs(3).Allowed = %v, want [3 1500 2047] (a fallback to 0..NumCPU-1 means the read failed)", p.Allowed)
	}
}

// PinToCPU(1500) set a bit of a 1024-bit set that is not there, so it asked
// the kernel for an empty mask (EINVAL) and the pin failed on a host whose CPU
// 1500 is where the plan put the loop.
func TestPinToCPUAboveTheFirst1024(t *testing.T) {
	k := installFakeKernel(t, 2048, 0, 1, 1500)
	if err := PinToCPU(1500); err != nil {
		t.Fatalf("PinToCPU(1500) on a 2048-CPU kernel: %v", err)
	}
	if got := k.cpus(); !slices.Equal(got, []int{1500}) {
		t.Errorf("after PinToCPU(1500) the mask is %v, want [1500]", got)
	}
	// Past what any kernel has: refused, and nothing allocated for it.
	if err := PinToCPU(1 << 30); !errors.Is(err, unix.EINVAL) {
		t.Errorf("PinToCPU(1<<30) = %v, want EINVAL", err)
	}
}

func (k *fakeKernel) cpus() []int { return cpusOf(k.mask) }

func cpusOf(mask []byte) []int {
	var out []int
	for cpu := range len(mask) * 8 {
		if mask[cpu/8]&(1<<(cpu%8)) != 0 {
			out = append(out, cpu)
		}
	}
	return out
}
