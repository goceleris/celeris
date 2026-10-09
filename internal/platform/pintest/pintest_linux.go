//go:build linux

package pintest

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/platform"
)

// Thread is one task of this process as /proc/self/task/<tid>/status shows it.
type Thread struct {
	TID  int
	Name string
	// CPUs is the Cpus_allowed_list line as the kernel prints it, e.g. "0-7"
	// or "3". The kernel prints the ranges canonically, so two masks are
	// equal exactly when these strings are.
	CPUs string
}

// Single reports whether the thread may run on exactly one CPU.
func (th Thread) Single() bool { return CountCPUs(th.CPUs) == 1 }

// IOUringKernelThread reports whether the task is one of the kernel's own
// io_uring threads (iou-wrk-<tid>, iou-sqp-<tid>). The kernel creates them in
// the process and sets their affinity itself; Go never runs a goroutine on
// one, so they are not what celeris#905 is about.
func (th Thread) IOUringKernelThread() bool { return strings.HasPrefix(th.Name, "iou-") }

func (th Thread) String() string {
	return fmt.Sprintf("tid=%d name=%q cpus=%s", th.TID, th.Name, th.CPUs)
}

// CountCPUs counts the CPUs in a Cpus_allowed_list string ("0-3,8" is 5). It
// returns 0 for a string it cannot parse.
func CountCPUs(list string) int {
	n := 0
	for part := range strings.SplitSeq(strings.TrimSpace(list), ",") {
		if part == "" {
			continue
		}
		lo, hi, isRange := strings.Cut(part, "-")
		a, err := strconv.Atoi(lo)
		if err != nil {
			return 0
		}
		b := a
		if isRange {
			if b, err = strconv.Atoi(hi); err != nil || b < a {
				return 0
			}
		}
		n += b - a + 1
	}
	return n
}

// readStatus returns the Name and Cpus_allowed_list lines of one status file.
func readStatus(path string) (name, cpus string, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", "", err
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		switch k {
		case "Name":
			name = strings.TrimSpace(v)
		case "Cpus_allowed_list":
			cpus = strings.TrimSpace(v)
		}
	}
	if cpus == "" {
		return "", "", fmt.Errorf("%s: no Cpus_allowed_list line", path)
	}
	return name, cpus, nil
}

// ProcessMask returns the Cpus_allowed_list of the thread-group leader (the
// main thread). Read before anything in the process has changed a thread's
// affinity, it is the mask every thread of the process starts with; read
// later, it may not be: the main thread runs goroutines too, an engine loop
// among them, and celeris#905 left it pinned. StartupMask is that early read.
func ProcessMask() (string, error) {
	_, cpus, err := readStatus("/proc/self/status")
	return cpus, err
}

// startup is the main thread's CPU mask when this package was initialised,
// before any test of the binary ran and so before anything in the process
// could have changed a thread's affinity. The runtime runs package
// initialisation on the main thread.
var startup struct {
	mask string // its Cpus_allowed_list
	set  unix.CPUSet
	err  error
}

func init() {
	if startup.mask, startup.err = ProcessMask(); startup.err == nil {
		startup.err = unix.SchedGetaffinity(0, &startup.set)
	}
}

// StartupMask returns the Cpus_allowed_list the main thread had when this
// package was initialised, before any test of the binary ran.
func StartupMask() (string, error) { return startup.mask, startup.err }

// Census returns every task of this process, sorted by TID. A task that exits
// while the census reads it is left out.
func Census() ([]Thread, error) {
	ents, err := os.ReadDir("/proc/self/task")
	if err != nil {
		return nil, err
	}
	out := make([]Thread, 0, len(ents))
	for _, ent := range ents {
		tid, err := strconv.Atoi(ent.Name())
		if err != nil {
			continue
		}
		name, cpus, err := readStatus("/proc/self/task/" + ent.Name() + "/status")
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) || errors.Is(err, unix.ESRCH) {
				continue
			}
			return nil, err
		}
		out = append(out, Thread{TID: tid, Name: name, CPUs: cpus})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TID < out[j].TID })
	return out, nil
}

// Off returns the tasks other than the kernel's io_uring threads whose mask is
// not want: the threads Go can schedule on, and the main thread even once the
// runtime has parked it for good.
func Off(threads []Thread, want string) []Thread {
	var off []Thread
	for _, th := range threads {
		if !th.IOUringKernelThread() && th.CPUs != want {
			off = append(off, th)
		}
	}
	return off
}

// Locked is what one goroutine saw on the OS thread it had locked.
type Locked struct {
	TID  int
	CPUs string // the thread's Cpus_allowed_list while the goroutine held it
}

// LockedThreads starts n goroutines that each lock an OS thread and hold it
// until all n hold one, then returns what each saw on its thread. A goroutine
// that holds its thread while it waits keeps that thread from every other
// goroutine, so the n goroutines are on n distinct threads, and the runtime
// takes an idle thread before it creates a new one: once n is larger than the
// number of threads the process has, every idle thread the runtime could hand
// a goroutine to is among them.
func LockedThreads(n int) ([]Locked, error) {
	var (
		mu     sync.Mutex
		seen   = make([]Locked, 0, n)
		errs   []error
		all    sync.WaitGroup
		done   sync.WaitGroup
		holdUp = make(chan struct{})
	)
	all.Add(n)
	for range n {
		done.Go(func() {
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			tid := unix.Gettid()
			_, cpus, err := readStatus("/proc/self/task/" + strconv.Itoa(tid) + "/status")
			mu.Lock()
			if err != nil {
				errs = append(errs, err)
			} else {
				seen = append(seen, Locked{TID: tid, CPUs: cpus})
			}
			mu.Unlock()
			all.Done()
			<-holdUp // hold the thread until every goroutine holds one
		})
	}
	all.Wait()
	close(holdUp)
	done.Wait()
	sort.Slice(seen, func(i, j int) bool { return seen[i].TID < seen[j].TID })
	return seen, errors.Join(errs...)
}

// envChild names the test a re-executed process is to run in its own process.
const envChild = "CELERIS_PINTEST_CHILD"

// envParentMask carries the StartupMask of the process that ran
// RunInOwnProcess to the process it started.
const envParentMask = "CELERIS_PINTEST_PARENT_MASK"

// envMask carries the CPU list RunInOwnProcessOn narrowed the child to.
const envMask = "CELERIS_PINTEST_MASK"

// InOwnProcess reports whether this process is the one RunInOwnProcess
// started for t.
func InOwnProcess(t *testing.T) bool { return os.Getenv(envChild) == t.Name() }

// RunInOwnProcess runs t again, alone, in a new process of the same test
// binary, and passes, skips or fails as that run did. The census above is of
// the whole process, and other tests in a package leave threads behind or
// still running; a process that runs nothing but t has only t's threads.
//
// The child's output is logged with every line prefixed "child| ", so that
// no line of it starts with "--- " or "=== " and no tally of this binary's
// output can count the child's lines as this process's results.
//
// The child starts with the CPU mask of the thread that forks it, and in a
// package whose earlier tests left a thread pinned to one CPU (celeris#905)
// that thread can be the one running t. So the fork is made from a thread t
// holds, and t fails if that thread's mask is not the one this process
// started with. The mask is put back before the fork, so the child still
// tests its own engine, and the child checks it again (envParentMask).
func RunInOwnProcess(t *testing.T, timeout time.Duration) {
	t.Helper()
	runInOwnProcess(t, timeout, nil)
}

// RunInOwnProcessOn is RunInOwnProcess for a child that starts restricted to
// the CPUs in cpus, as if started under taskset: the thread that forks it is
// narrowed to cpus for the fork, then given back the process's mask. The
// child sees the mask as StartupMask and as envMask, and checks it
// (LoopsStayInsideTheMask).
func RunInOwnProcessOn(t *testing.T, timeout time.Duration, cpus []int) {
	t.Helper()
	if len(cpus) == 0 {
		t.Fatal("RunInOwnProcessOn: no CPUs")
	}
	runInOwnProcess(t, timeout, cpus)
}

func runInOwnProcess(t *testing.T, timeout time.Duration, narrow []int) {
	t.Helper()
	if InOwnProcess(t) {
		t.Fatal("RunInOwnProcess called from inside the process it started")
	}
	if startup.err != nil {
		t.Fatalf("read the process's CPU mask at init: %v", startup.err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), timeout+30*time.Second)
	defer cancel()
	name := t.Name()
	cmd := exec.CommandContext(ctx, os.Args[0],
		"-test.run=^"+regexp.QuoteMeta(name)+"$",
		"-test.count=1",
		"-test.v",
		"-test.timeout="+timeout.String(),
	)
	cmd.Env = append(os.Environ(), envChild+"="+name, envParentMask+"="+startup.mask)
	var narrowSet unix.CPUSet
	if len(narrow) > 0 {
		for _, c := range narrow {
			narrowSet.Set(c)
		}
		cmd.Env = append(cmd.Env, envMask+"="+platform.FormatCPUs(narrow))
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	runtime.LockOSThread()
	var cur unix.CPUSet
	if err := unix.SchedGetaffinity(0, &cur); err != nil {
		runtime.UnlockOSThread()
		t.Fatalf("sched_getaffinity: %v", err)
	}
	if cur != startup.set {
		_, cpus, _ := readStatus("/proc/thread-self/status")
		t.Errorf("celeris#905 in this process: the thread %s runs on (tid %d) may run on CPU %s only, not on "+
			"the %s this process started with -- an earlier test left a thread pinned and handed it back to "+
			"the scheduler", name, unix.Gettid(), cpus, startup.mask)
		if err := unix.SchedSetaffinity(0, &startup.set); err != nil {
			t.Errorf("put the process's mask back on that thread before the fork: %v", err)
		}
	}
	if len(narrow) > 0 {
		if err := unix.SchedSetaffinity(0, &narrowSet); err != nil {
			runtime.UnlockOSThread()
			t.Fatalf("narrow the forking thread to CPUs %s: %v", platform.FormatCPUs(narrow), err)
		}
	}
	err := cmd.Start()
	if len(narrow) > 0 {
		// Give the thread its mask back before anything else runs on it.
		if rerr := unix.SchedSetaffinity(0, &startup.set); rerr != nil {
			t.Errorf("put the process's mask back on the forking thread: %v", rerr)
		}
	}
	runtime.UnlockOSThread()
	if err == nil {
		err = cmd.Wait()
	}
	var b strings.Builder
	for line := range strings.SplitSeq(strings.TrimRight(out.String(), "\n"), "\n") {
		b.WriteString("child| ")
		b.WriteString(line)
		b.WriteByte('\n')
	}
	pid := -1
	if cmd.ProcessState != nil {
		pid = cmd.ProcessState.Pid()
	}
	t.Logf("%s in its own process (pid %d):\n%s", name, pid, b.String())

	passed := regexp.MustCompile(`(?m)^--- PASS: ` + regexp.QuoteMeta(name) + ` \(`).Match(out.Bytes())
	skipped := regexp.MustCompile(`(?m)^--- SKIP: ` + regexp.QuoteMeta(name) + ` \(`).Match(out.Bytes())
	switch {
	case err == nil && passed:
	case err == nil && skipped:
		t.Skipf("%s skipped in its own process (its reason is in the log above)", name)
	default:
		t.Fatalf("%s failed in its own process: err=%v, PASS line %v (output above)", name, err, passed)
	}
}

// MinPinnedLoops returns how many of loops engine loops are pinned to one CPU
// at least: all of them, unless the allowed CPUs differ in capacity, where
// the loops beyond the big CPUs run unpinned (platform.PlanWorkerCPUs).
func MinPinnedLoops(loops int) int {
	if loops < 1 {
		return 0
	}
	return platform.PlanWorkerCPUs(loops).Pinned()
}

// StartFunc starts one engine and returns once it serves. It returns the
// number of loop threads the engine runs (each pins its thread to one CPU) and
// a stop function that returns once the engine's Listen has returned.
type StartFunc func(t *testing.T) (loops int, stop func())

// exitBound is how long a stopped engine's threads get to leave the process
// or get their mask back: the runtime ends a thread as soon as the goroutine
// locked to it exits, so this is slack, not a latency anybody measured.
const exitBound = 5 * time.Second

// StoppedEnginesLeaveNoPinnedThread is the body of the celeris#905
// regression tests. Call it from the process RunInOwnProcess started. It
// starts and stops an engine cycles times in this process, and after every
// stop requires that
//
//   - no task of the process but the kernel's io_uring threads has a CPU mask
//     other than the one the process started with, within exitBound. That is
//     every thread Go can schedule on, so none is left pinned to one CPU, and
//     also the main thread once the runtime has parked it for good: Go never
//     runs it again, but its mask is what /proc/<pid>/status and taskset -p
//     report for the whole process;
//   - every thread but the main thread that was pinned while the engine ran
//     has left the process, within exitBound. A loop's goroutine exits locked
//     to its thread, so the runtime ends the thread rather than hand it, and
//     the other state the loop put on it, to the next goroutine; and
//   - a goroutine that locks an OS thread afterwards sees that mask, on every
//     thread the runtime can give it (LockedThreads).
//
// The engine must have pinned a thread while it ran (one per loop): a run in
// which nothing was pinned checks nothing, and fails. So does a run in which
// every pinned thread was the main thread. With two or more cycles that cannot
// happen to the fix: the runtime parks the main thread for good once a loop
// has exited locked on it, so a later cycle's loops run on other threads.
//
// The fix's restore matters only for the main thread (every other loop
// thread exits), and a loop runs on the main thread only in some runs: a run
// in which none did says so in its log and its RESULT line.
func StoppedEnginesLeaveNoPinnedThread(t *testing.T, engine string, cycles int, start StartFunc) {
	t.Helper()
	base, err := StartupMask()
	if err != nil {
		t.Fatalf("read the process's CPU mask at init: %v", err)
	}
	// The parent's mask decides the skip: a child forked from a thread left
	// pinned would otherwise skip as a one-CPU process (celeris#905 in the
	// parent), where it has to fail.
	parent := os.Getenv(envParentMask)
	if parent == "" {
		parent = base
	}
	if n := CountCPUs(parent); n < 2 {
		t.Skipf("the process may run on CPU %s only (%d CPU): a thread pinned to one CPU cannot be told "+
			"from one that is not", parent, n)
	}
	if base != parent {
		t.Fatalf("celeris#905 in the parent process: this process started on CPU mask %s, but the process "+
			"that forked it started on %s -- it was forked from a thread left pinned and handed back to the "+
			"scheduler", base, parent)
	}
	before, err := Census()
	if err != nil {
		t.Fatalf("census: %v", err)
	}
	if off := Off(before, base); len(off) > 0 {
		t.Fatalf("premise: before any engine ran, %d thread(s) were already off the process mask %s: %v",
			len(off), base, off)
	}
	pid := os.Getpid()
	var pinnedRunning, leftOff, lockedProbe, lockedOff, m0Hosted, pinnedNotMain, leftAlive int
	for cycle := 1; cycle <= cycles; cycle++ {
		// The main thread counts as hosting a loop only if it had the
		// process's mask when the cycle began, not if an earlier cycle left
		// it pinned.
		m0Before, err := ProcessMask()
		if err != nil {
			t.Fatalf("cycle %d: read the main thread's mask: %v", cycle, err)
		}
		loops, stop := start(t)
		running, err := Census()
		if err != nil {
			stop()
			t.Fatalf("cycle %d: census while running: %v", cycle, err)
		}
		var pinned, notMain []Thread
		for _, th := range Off(running, base) {
			if th.Single() {
				pinned = append(pinned, th)
				if th.TID != pid {
					notMain = append(notMain, th)
				} else if m0Before == base {
					m0Hosted++
				}
			}
		}
		pinnedRunning += len(pinned)
		pinnedNotMain += len(notMain)
		t.Logf("cycle %d: %s engine running with %d loop(s); %d of %d threads pinned to one CPU: %v",
			cycle, engine, loops, len(pinned), len(running), pinned)
		// Every loop pins, except on a host whose allowed CPUs differ in
		// capacity (arm64 big.LITTLE), where the engine leaves the loops that
		// do not fit on the big CPUs unpinned (celeris#909). Several engines
		// (adaptive) plan independently, each pinning at least
		// min(its loops, the big CPUs), so min(loops, big CPUs) is a floor.
		wantPinned := MinPinnedLoops(loops)
		if loops < 1 || len(pinned) < wantPinned {
			stop()
			t.Fatalf("premise: the %s engine runs %d loop(s) of which at least %d are pinned to one CPU, but only "+
				"%d thread(s) were pinned while it ran (process mask %s) -- the engine no longer pins, or "+
				"the pin failed here, and this test would check nothing", engine, loops, wantPinned, len(pinned), base)
		}

		stop()

		var off, alive []Thread
		for deadline := time.Now().Add(exitBound); ; {
			after, err := Census()
			if err != nil {
				t.Fatalf("cycle %d: census after stop: %v", cycle, err)
			}
			off, alive = Off(after, base), stillThere(after, notMain)
			if len(off)+len(alive) == 0 || time.Now().After(deadline) {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		leftOff += len(off)
		leftAlive += len(alive)
		if len(off) > 0 {
			t.Errorf("cycle %d: %v after the %s engine stopped, %d thread(s) of the process are still off "+
				"its mask %s (the scheduler can run any goroutine on such a thread; a parked main thread "+
				"makes the whole process read as pinned): %v",
				cycle, exitBound, engine, len(off), base, off)
		}
		if len(alive) > 0 {
			t.Errorf("cycle %d: %v after the %s engine stopped, %d of the %d thread(s) other than the main "+
				"thread that were pinned while it ran are still threads of the process (a loop that unlocks "+
				"its thread hands it, and the state the loop put on it, back to the scheduler): %v",
				cycle, exitBound, engine, len(alive), len(notMain), alive)
		}

		threads, err := Census()
		if err != nil {
			t.Fatalf("cycle %d: census: %v", cycle, err)
		}
		n := len(threads) + 16
		seen, err := LockedThreads(n)
		if err != nil {
			t.Fatalf("cycle %d: locked-thread probe: %v", cycle, err)
		}
		lockedProbe += len(seen)
		var bad []Locked
		for _, s := range seen {
			if s.CPUs != base {
				bad = append(bad, s)
			}
		}
		lockedOff += len(bad)
		if len(seen) != n {
			t.Errorf("cycle %d: %d of %d probe goroutines reported", cycle, len(seen), n)
		}
		if len(bad) > 0 {
			t.Errorf("cycle %d: after the %s engine stopped, %d of %d goroutines that locked an OS thread "+
				"got one whose mask is not the process's %s: %v", cycle, engine, len(bad), len(seen), base, bad)
		}
	}
	if m0Hosted == 0 {
		t.Logf("in %d cycle(s) no loop ran on the main thread, so this run did not check the restore that "+
			"leaves the main thread the process's mask (TestSaveThreadAffinityRestoresThePin covers Restore "+
			"itself)", cycles)
	}
	if pinnedNotMain == 0 {
		t.Errorf("premise: in %d cycle(s) every thread pinned while the %s engine ran was the main thread, so "+
			"this run did not check that a stopped loop's thread leaves the process", cycles, engine)
	}
	t.Logf("celeris905 RESULT engine=%s cycles=%d process_mask=%s pinned_while_running=%d "+
		"main_thread_hosted_a_loop=%d off_mask_after_stop=%d locked_probes=%d locked_off_mask=%d "+
		"pinned_not_main=%d alive_after_stop=%d",
		engine, cycles, base, pinnedRunning, m0Hosted, leftOff, lockedProbe, lockedOff, pinnedNotMain, leftAlive)
}

// stillThere returns the threads of want that are still among threads, by TID.
func stillThere(threads, want []Thread) []Thread {
	var out []Thread
	for _, w := range want {
		for _, th := range threads {
			if th.TID == w.TID {
				out = append(out, th)
				break
			}
		}
	}
	return out
}

// AllowedCPUs returns the members of the CPU mask this process started with.
func AllowedCPUs() ([]int, error) {
	if startup.err != nil {
		return nil, startup.err
	}
	var out []int
	for c := 0; c < len(startup.set)*64; c++ {
		if startup.set.IsSet(c) {
			out = append(out, c)
		}
	}
	return out, nil
}

// MaskWithoutTheLowestCPU returns the process's allowed CPUs except the lowest,
// the mask RunInOwnProcessOn is given to test celeris#909: an engine that pins
// loop i to CPU index i pins loop 0 to a CPU the mask excludes. It skips the
// test when the process may use fewer than 3 CPUs, where such a mask cannot
// tell a loop pinned inside it from one pinned outside it.
func MaskWithoutTheLowestCPU(t *testing.T) []int {
	t.Helper()
	cpus, err := AllowedCPUs()
	if err != nil {
		t.Fatalf("read the process's CPU mask: %v", err)
	}
	if len(cpus) < 3 {
		t.Skipf("the process may use %d CPU(s) (%s): a mask that leaves out the lowest of them and still has two "+
			"members needs 3", len(cpus), platform.FormatCPUs(cpus))
	}
	return cpus[1:]
}

// LoopsStayInsideTheMask is the body of the celeris#909 tests. Call it from the
// process RunInOwnProcessOn started. It starts one engine and checks, from
// /proc/self/task/*/status, that
//   - the process really started on the mask it was given (the premise);
//   - every thread pinned to one CPU is pinned to a member of the mask;
//   - at least as many threads are pinned as the plan pins (all loops, but on a
//     host whose allowed CPUs differ in capacity the loops beyond the big CPUs
//     run unpinned), and
//   - on a host whose allowed CPUs are alike, the engine's loops took the first
//     CPUs of the mask, in order, which is "the i-th member of the allowed
//     set" written out independently of the planner.
func LoopsStayInsideTheMask(t *testing.T, engine string, start StartFunc) {
	t.Helper()
	want := os.Getenv(envMask)
	if want == "" {
		t.Fatal("premise: LoopsStayInsideTheMask must run in the process RunInOwnProcessOn started")
	}
	base, err := StartupMask()
	if err != nil {
		t.Fatalf("read the process's CPU mask: %v", err)
	}
	if base != want {
		t.Fatalf("premise: the process started on CPU mask %s, not the %s it was narrowed to", base, want)
	}
	members, err := AllowedCPUs()
	if err != nil || len(members) < 2 {
		t.Fatalf("premise: mask %s has members %v (%v)", base, members, err)
	}
	loops, stop := start(t)
	defer stop()
	running, err := Census()
	if err != nil {
		t.Fatalf("census: %v", err)
	}
	var pinned []Thread
	inMask := map[int]bool{}
	for _, c := range members {
		inMask[c] = true
	}
	for _, th := range running {
		if th.IOUringKernelThread() || !th.Single() {
			continue
		}
		pinned = append(pinned, th)
	}
	got := map[int]bool{}
	for _, th := range pinned {
		c, err := strconv.Atoi(th.CPUs)
		if err != nil {
			t.Fatalf("thread %v: cannot read its CPU", th)
		}
		got[c] = true
		if !inMask[c] {
			t.Errorf("celeris#909: the %s engine pinned a thread to CPU %d, outside the process's mask %s: %v",
				engine, c, base, th)
		}
	}
	plan := platform.PlanWorkerCPUs(loops)
	if need := plan.Pinned(); len(pinned) < need {
		t.Errorf("the %s engine runs %d loop(s) of which %d should be pinned, but %d thread(s) are pinned to one CPU "+
			"(mask %s): %v", engine, loops, need, len(pinned), base, pinned)
	}
	if !plan.Heterogeneous {
		exp := map[int]bool{}
		for i := range loops {
			exp[members[i%len(members)]] = true
		}
		if !reflect.DeepEqual(exp, got) {
			t.Errorf("celeris#909: the %s engine's %d loop(s) are pinned to CPUs %v, want the first %d member(s) of the mask %s: %v",
				engine, loops, platform.FormatCPUs(keys(got)), min(loops, len(members)), base, platform.FormatCPUs(keys(exp)))
		}
	}
	t.Logf("celeris909 RESULT engine=%s mask=%s loops=%d pinned=%d pinned_cpus=%s heterogeneous=%v",
		engine, base, loops, len(pinned), platform.FormatCPUs(keys(got)), plan.Heterogeneous)
}

func keys(m map[int]bool) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}
