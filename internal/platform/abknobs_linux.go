//go:build linux

package platform

// Throwaway diagnostic knobs for the celeris#1036 stall investigation (X2). NOT for merge, not product code:
// every knob is an environment variable read when an engine starts, and every knob is off when unset.
//
//	CELERIS_AB_EXCLUDE_CPU=8    no loop on these CPUs (cpuplan_linux.go, abExcludeCPUs)
//	CELERIS_AB_EXTRA_LOOP_CPU=4 one more loop pinned on these CPUs, even a little one (abExtraLoopCPUs)
//	CELERIS_AB_SPIN_CPUS=4      a SCHED_IDLE spinner thread on these CPUs keeps them out of idle (ABStartSpinners)
//	CELERIS_AB_BUSYPOLL=0       accepted sockets get no SO_BUSY_POLL (internal/sockopts)

import (
	"context"
	"log/slog"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/unix"
)

func abParseCPUs(name string) []int {
	v := os.Getenv(name)
	if v == "" {
		return nil
	}
	var out []int
	for _, f := range strings.Split(v, ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(f)); err == nil && n >= 0 {
			out = append(out, n)
		}
	}
	return out
}

// abExtraLoopCPUs appends the CPUs named by CELERIS_AB_EXTRA_LOOP_CPU that the process may run on and that are not
// in cpus yet. With the default worker count the engine then starts one loop per member (9 on msr1 with 4).
func abExtraLoopCPUs(cpus, allowed []int) []int {
	extra := abParseCPUs("CELERIS_AB_EXTRA_LOOP_CPU")
	if len(extra) == 0 {
		return cpus
	}
	out := append([]int(nil), cpus...)
	for _, c := range extra {
		in := false
		for _, a := range allowed {
			if a == c {
				in = true
			}
		}
		for _, o := range out {
			if o == c {
				in = false
			}
		}
		if in {
			out = append(out, c)
		}
	}
	return out
}

var (
	abSpinOnce sync.Once
	abSink     atomic.Uint64
)

// ABStartSpinners starts, once per process, one spinner thread per CPU of CELERIS_AB_SPIN_CPUS. A spinner locks its
// OS thread, pins it to its CPU, sets SCHED_IDLE (nice 19 if the kernel refuses) and burns CPU in user space without a
// system call, so the CPU never reaches its idle state but any other work preempts it at once. It ends with ctx.
func ABStartSpinners(ctx context.Context) {
	cpus := abParseCPUs("CELERIS_AB_SPIN_CPUS")
	if len(cpus) == 0 {
		return
	}
	abSpinOnce.Do(func() {
		for _, c := range cpus {
			go abSpin(ctx, c)
		}
	})
}

func abSpin(ctx context.Context, cpu int) {
	runtime.LockOSThread() // never unlocked: the thread ends with this goroutine
	if err := PinToCPU(cpu); err != nil {
		slog.Warn("ab spinner: pin failed", "cpu", cpu, "err", err)
		return
	}
	attr := unix.SchedAttr{Size: uint32(unsafe.Sizeof(unix.SchedAttr{})), Policy: unix.SCHED_IDLE}
	policy := "SCHED_IDLE"
	if err := unix.SchedSetAttr(0, &attr, 0); err != nil {
		policy = "nice19"
		if err2 := unix.Setpriority(unix.PRIO_PROCESS, unix.Gettid(), 19); err2 != nil {
			policy = "none"
		}
	}
	slog.Info("ab spinner started", "cpu", cpu, "policy", policy, "tid", unix.Gettid())
	var x uint64
	for ctx.Err() == nil {
		for i := 0; i < 1<<18; i++ {
			x += uint64(i) ^ (x >> 3)
		}
		abSink.Store(x)
	}
}
