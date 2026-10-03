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
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
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
	err := cmd.Start()
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
		if loops < 1 || len(pinned) < loops {
			stop()
			t.Fatalf("premise: the %s engine runs %d loop(s) but only %d thread(s) were pinned to one CPU "+
				"while it ran (process mask %s) -- the engine no longer pins, or the pin failed here, "+
				"and this test would check nothing", engine, loops, len(pinned), base)
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
