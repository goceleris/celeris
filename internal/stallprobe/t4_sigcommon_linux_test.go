//go:build linux

package stallprobe

// Shared pieces of the signal-path probes E0 (t5), E3 (t6) and E4 (t7), which
// test the mechanism proposed for the msr1 stalls (celeris#945): a GC root
// scan's suspendG keeps sending SIGURG to a locked, pinned loop goroutine on a
// Cortex-A520, and the thread never gets out of signal handling.
//
// What the runtime does (go1.27 preempt.go suspendG, signal_unix.go): to
// preempt a goroutine seen _Grunning the sender calls preemptM, which is a
// tgkill(SIGURG) to the target's thread (signalM). The target's Go handler
// runs doSigPreempt, which, when the goroutine is at an async safe point,
// injects asyncPreempt, and in every case ACKNOWLEDGES: preemptGen++ and
// signalPending=0. suspendG spins (procyield, sched_yield every ~5 us). It
// re-sends only if the acknowledge was NOT seen before the rate limit
// (yieldDelay/2 = 5 us) expired: a signal whose send-to-acknowledge latency is
// below 5 us is dropped for good, so a signal storm needs a per-signal latency
// of about 5 us or more on the target. The probes measure that latency per
// CPU class.
//
// All three probes send the SAME signal by the SAME call the runtime makes
// (tgkill(SIGURG) to a Go-locked thread, run by Go's own handler with
// GODEBUG asyncpreemptoff=0, which they set explicitly: with
// asyncpreemptoff=1 the handler runs but does not acknowledge). No other
// signal, no os/signal, no ptrace, no perf. Pinned threads exist only in
// child processes (runChild) that end with the parent.
//
// Knobs (all optional; the stress workflow's extra input takes NAME=VALUE):
//
//	E0  CELERIS_PROBE_E0_SAMPLES   cross-CPU samples per (sender, target) pair (10000)
//	    CELERIS_PROBE_E0_SELF_N    self-signal iterations per CPU, time-capped at 2 s (100000)
//	    CELERIS_PROBE_E0_SENDERS   class (the fastest and the slowest CPU of every class) or all (every CPU) (class)
//	    CELERIS_PROBE_E0_BUDGET_S  stop starting pairs after this many seconds (240)
//	E3  CELERIS_PROBE_E3_RATES     per-target injection rates, signals/s (1000,10000,50000,100000,200000)
//	    CELERIS_PROBE_E3_SECS      seconds per rate (3)
//	    CELERIS_PROBE_E3_LOOPS     loop threads per class, one signalled, the rest bystanders (4)
//	    CELERIS_PROBE_E3_WORK_US   user work per timeout wake-up, us (20)
//	E4  CELERIS_PROBE_E4_CASES     slow,fast,all (+ slowconf: loops on the slow class AND the process mask confined to them) (slow,fast,all)
//	    CELERIS_PROBE_E4_MODES     default,noasync (both)
//	    CELERIS_PROBE_E4_SECS      seconds per child (30)
//	    CELERIS_PROBE_E4_ROUNDS    fresh children per (case, mode) (1)
//	    CELERIS_PROBE_E4_FAIL_MS   a default-mode loop wake gap above this fails the test (1000)
//	    CELERIS_PROBE_E4_WORK_US   user work per wake-up, us (20)
//	    CELERIS_PROBE_E4_WORK_MODE iters = a fixed iteration count that takes WORK_US on the FASTEST core and proportionally longer on a slower one, as the A520 loops
//	                               of the celeris probe spent ~12x the CPU per wake-up of the A720 ones (49 us vs 4.2 us); time = WORK_US of wall time on every core (iters)
//	    CELERIS_PROBE_E4_GRACE_S   how long past SECS the parent waits for a child before it kills it and fails the subtest (120)
//	    CELERIS_PROBE_E4_ALLOCS    allocator goroutines (4)
//	    CELERIS_PROBE_E4_ALLOC_MBS total garbage rate, MiB/s (256)
//	    CELERIS_PROBE_E4_LIVE_MB   live pointer-bearing heap, MiB (32)
//	    CELERIS_PROBE_E4_GOGC      GOGC of the child (100)
//	    CELERIS_PROBE_E4_GCTRACE   1 = GODEBUG gctrace=1, the last lines of the child's stderr are logged (0)
//	    CELERIS_PROBE_E4_INJECT_HANG_MS proof knob, off (0): loop 0 sleeps this long once, INJECT_AT_S (2) seconds in; a default-mode run must then FAIL
//	    CELERIS_PROBE_E4_INJECT_WEDGE   proof knob, off (0): 1 = the child's main goroutine never reports; the parent must time it out and print the heartbeat table (use a small GRACE_S)

import (
	"bytes"
	"fmt"
	"os"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

// sigChildren are the child kinds of the signal-path probes, dispatched by
// runChildMain.
var sigChildren = map[string]func() (any, error){
	"e0": e0Child,
	"e3": e3Child,
	"e4": e4Child,
}

// rawTgkill is the runtime's own call (signalM): tgkill(pid, tid, sig) with no
// entersyscall.
func rawTgkill(pid, tid int, sig unix.Signal) {
	_, _, _ = unix.RawSyscall(unix.SYS_TGKILL, uintptr(pid), uintptr(tid), uintptr(sig))
}

func rawNullSyscall() {
	_, _, _ = unix.RawSyscall(unix.SYS_GETPPID, 0, 0, 0)
}

var spinSink atomic.Uint64

// spinFor busy-waits ns nanoseconds of user code.
func spinFor(c clk, ns int64) {
	if ns <= 0 {
		return
	}
	end := c.now() + ns
	var n uint64
	for c.now() < end {
		n++
	}
	spinSink.Add(n)
}

// spinIters is a fixed amount of work: it takes longer on a slower core.
//
//go:noinline
func spinIters(n int) uint64 {
	x := uint64(88172645463325252)
	for range n {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
	}
	return x
}

// calibrateIters returns how many spinIters iterations take ns on this thread
// (the caller pins it to the CPU to calibrate on) and the ns per iteration.
func calibrateIters(c clk, ns int64) (iters int, nsPer float64) {
	best := 1e18
	for range 5 {
		t0 := c.now()
		spinSink.Add(spinIters(2_000_000))
		best = min(best, float64(c.now()-t0)/2e6)
	}
	return max(int(float64(ns)/best), 1), best
}

func classLabel(tp topo, cpu int) string {
	if !tp.Hetero {
		return "all"
	}
	return tp.CPU[cpu].class()
}

// classIndex returns the index in tp.Classes of the class holding cpu.
func classIndex(tp topo, cpu int) int {
	for i, g := range tp.Classes {
		if slices.Contains(g, cpu) {
			return i
		}
	}
	return 0
}

func sortedCopy(c []int) []int {
	out := append([]int(nil), c...)
	sort.Ints(out)
	return out
}

func takeN(c []int, n int) []int {
	if n > len(c) {
		n = len(c)
	}
	return append([]int(nil), c[:n]...)
}

func hostFacts() string {
	var u unix.Utsname
	_ = unix.Uname(&u)
	b := func(a [65]byte) string { return strings.TrimRight(string(a[:]), "\x00") }
	return fmt.Sprintf("%s %s %s GOARCH=%s %s GOMAXPROCS=%d NumCPU=%d", b(u.Sysname), b(u.Release), b(u.Machine), runtime.GOARCH, runtime.Version(), runtime.GOMAXPROCS(0), runtime.NumCPU())
}

// ---- percentile helpers ----------------------------------------------------

type pcts struct {
	N                   int
	P50, P90, P99, P999 int64
	Max                 int64
	Mean                float64
}

func percentiles(v []int64) pcts {
	var p pcts
	p.N = len(v)
	if len(v) == 0 {
		return p
	}
	s := append([]int64(nil), v...)
	slices.Sort(s)
	at := func(q float64) int64 {
		i := int(q * float64(len(s)-1))
		return s[i]
	}
	p.P50, p.P90, p.P99, p.P999, p.Max = at(0.5), at(0.9), at(0.99), at(0.999), s[len(s)-1]
	var sum float64
	for _, x := range s {
		sum += float64(x)
	}
	p.Mean = sum / float64(len(s))
	return p
}

func us(ns int64) string    { return fmt.Sprintf("%.2f", float64(ns)/1e3) }
func usF(ns float64) string { return fmt.Sprintf("%.2f", ns/1e3) }

// ---- per-thread state from /proc ---------------------------------------------

type thrStat struct {
	State      byte
	Ut, St     int64 // clock ticks (USER_HZ = 100)
	Vol, Invol int64
	LastCPU    int
	OK         bool
}

func readThr(tid int) thrStat {
	var s thrStat
	b, err := os.ReadFile(fmt.Sprintf("/proc/self/task/%d/stat", tid))
	if err != nil {
		return s
	}
	i := bytes.LastIndexByte(b, ')')
	if i < 0 || i+2 >= len(b) {
		return s
	}
	f := strings.Fields(string(b[i+2:])) // f[0] is field 3 (state)
	if len(f) < 37 {
		return s
	}
	s.State = f[0][0]
	s.Ut, _ = strconv.ParseInt(f[11], 10, 64)
	s.St, _ = strconv.ParseInt(f[12], 10, 64)
	s.LastCPU, _ = strconv.Atoi(f[36])
	if st, err := os.ReadFile(fmt.Sprintf("/proc/self/task/%d/status", tid)); err == nil {
		for _, l := range strings.Split(string(st), "\n") {
			if v, ok := strings.CutPrefix(l, "voluntary_ctxt_switches:"); ok {
				s.Vol, _ = strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			} else if v, ok := strings.CutPrefix(l, "nonvoluntary_ctxt_switches:"); ok {
				s.Invol, _ = strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			}
		}
	}
	s.OK = true
	return s
}

// ---- the idle epoll loop ---------------------------------------------------

// loopStats is one pinned 1 ms-epoll loop thread. Every field is an atomic the
// loop writes and the measuring goroutine reads, so a loop that is stuck can be
// read (its open gap is now - LastAny / now - LastWake).
//
// A "return" is any return of epoll_wait. A "wake" is a return that is not
// EINTR: with the idle epfd it is the 1 ms timeout. celeris restarts the wait
// on EINTR (epoll/loop.go: `if err == unix.EINTR { continue }`), so every signal
// restarts the 1 ms timeout and a signal rate above 1 kHz keeps the wake count
// at zero by itself; the "any" gap is what shows a thread that is not running
// at all.
type loopStats struct {
	CPU        int
	WorkIters  int // > 0: user work is this many iterations of spinIters (core-speed dependent), else workNs of wall time
	Tid        atomic.Int64
	Wakes      atomic.Uint64
	Eintr      atomic.Uint64
	OtherErr   atomic.Uint64
	MaxGapAny  atomic.Int64 // ns, resettable with Swap(0)
	MaxGapWake atomic.Int64
	Gap50      atomic.Uint64 // wake gaps above 50 ms
	Gap500     atomic.Uint64
	Gap1s      atomic.Uint64
	LastAny    atomic.Int64 // absolute ns of the last return
	LastWake   atomic.Int64
	PinErr     atomic.Value // string
	Exited     atomic.Bool
	Hang       atomic.Int64 // proof knob: sleep this many ns once, after the next wake
	_          [64]byte
}

func atomicMax(a *atomic.Int64, v int64) {
	for {
		cur := a.Load()
		if v <= cur || a.CompareAndSwap(cur, v) {
			return
		}
	}
}

// run is the loop. It locks its thread and never unlocks it, so the thread
// ends with the goroutine; workNs of pure Go user code follows every wake.
func (ls *loopStats) run(c clk, workNs int64, stop *atomic.Bool, ready chan<- struct{}) {
	defer ls.Exited.Store(true)
	if e := pinThis(ls.CPU); e != "" {
		ls.PinErr.Store(e)
	}
	ls.Tid.Store(int64(unix.Gettid()))
	epfd, err := unix.EpollCreate1(unix.EPOLL_CLOEXEC)
	if err != nil {
		ls.PinErr.Store("epoll_create1: " + err.Error())
		ready <- struct{}{}
		return
	}
	defer func() { _ = unix.Close(epfd) }()
	evs := make([]unix.EpollEvent, 8)
	ready <- struct{}{}
	t := c.now()
	ls.LastAny.Store(t)
	ls.LastWake.Store(t)
	lastAny, lastWake := t, t
	for !stop.Load() {
		_, err := unix.EpollWait(epfd, evs, 1)
		t := c.now()
		atomicMax(&ls.MaxGapAny, t-lastAny)
		lastAny = t
		ls.LastAny.Store(t)
		if err != nil {
			if err == unix.EINTR {
				ls.Eintr.Add(1)
			} else {
				ls.OtherErr.Add(1)
			}
			continue
		}
		ls.Wakes.Add(1)
		g := t - lastWake
		lastWake = t
		ls.LastWake.Store(t)
		atomicMax(&ls.MaxGapWake, g)
		switch {
		case g > 1_000_000_000:
			ls.Gap1s.Add(1)
			ls.Gap500.Add(1)
			ls.Gap50.Add(1)
		case g > 500_000_000:
			ls.Gap500.Add(1)
			ls.Gap50.Add(1)
		case g > 50_000_000:
			ls.Gap50.Add(1)
		}
		if ls.WorkIters > 0 {
			spinSink.Add(spinIters(ls.WorkIters))
		} else {
			spinFor(c, workNs)
		}
		if h := ls.Hang.Swap(0); h > 0 {
			time.Sleep(time.Duration(h))
		}
	}
}

// startLoops starts one loop per cpu and waits (up to 15 s) until all are ready.
func startLoops(c clk, cpus []int, workNs int64, workIters int, stop *atomic.Bool) ([]*loopStats, error) {
	ls := make([]*loopStats, len(cpus))
	ready := make(chan struct{}, len(cpus))
	for i, cpu := range cpus {
		ls[i] = &loopStats{CPU: cpu, WorkIters: workIters}
		go ls[i].run(c, workNs, stop, ready)
	}
	to := time.After(15 * time.Second)
	for range cpus {
		select {
		case <-ready:
		case <-to:
			return ls, fmt.Errorf("the loop threads were not ready after 15 s")
		}
	}
	for _, l := range ls {
		if e, _ := l.PinErr.Load().(string); e != "" {
			return ls, fmt.Errorf("loop on cpu %d: %s", l.CPU, e)
		}
	}
	return ls, nil
}

// gcCycles reads the GC cycle counter without stopping the world.
func gcCycles() uint64 {
	return readSTW().cycles
}
