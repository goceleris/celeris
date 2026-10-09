//go:build linux

// Package stallprobe holds throwaway probes for the msr1 multi-second stalls
// (celeris#905, #945, probatorium#473). It has test files only and no product
// code. Every probe runs under the celeris-stress workflow
// (packages=./internal/stallprobe, run=<regexp>), on both arches, and talks
// only through t.Log and the pass/fail verdict.
//
// The measuring itself runs in CHILD processes, started by the test from its
// own binary (TestMain dispatches on CELERIS_PROBE_CHILD): a fresh process has
// none of the thread state an earlier test left behind (celeris#905), the
// runtime knobs that must be set before the Go runtime starts (GOMAXPROCS,
// GOGC, GODEBUG=asyncpreemptoff) go in its environment, and an affinity mask
// set on the parent's thread right before the fork is the child's from its
// first instruction, so runtime.NumCPU and GOMAXPROCS follow it. A child
// writes one JSON document to stdout; the parent turns it into logs and
// verdicts. Nothing here writes to stdout directly: the stress tally parses
// "--- PASS/FAIL/SKIP" lines and a glued or stray line makes a shard UNPARSED.
//
// Run order. Go runs the tests of a package in source order when the stress
// workflow is given extra='-shuffle=off' (it is on the workflow's allow-list;
// without it every shard gets its own random -shuffle seed), and it reads the
// files in name order: t1_p3 (P3, the network workload), t2_p2 (P2), t3_p1
// (P1 and P1b, which run every CPU at 100% for 60 s per count and heat-soak
// the SoC). The all-in-one dispatch therefore needs -shuffle=off to run P3
// before P1 on a cool host; P1 in a dispatch of its own is cleaner still.
//
// Knobs (the stress workflow's extra input accepts CELERIS_* names and the
// characters [A-Za-z0-9_.,:/+-] in values):
//
//	CELERIS_PROBE_SECONDS   P1/P1b measuring window, seconds (60)
//	CELERIS_PROBE_GAP_MS    P1: record every gap above this, ms (20)
//	CELERIS_PROBE_FAIL_MS   P1: fail on any gap above this, ms (500)
//	CELERIS_PROBE_CPUS      P1: cpu list to observe, e.g. 2-5 (every allowed CPU)
//	CELERIS_PROBE_OBSERVER  P1: 1 = also run the independent sleeping observer process (1)
//	CELERIS_PROBE_REPS      P2/P3: repetitions per leaf, summed over the rounds (100)
//	CELERIS_PROBE_MINREPS   P2/P3: a leaf does not stop for its stall budget before this many reps (40)
//	CELERIS_PROBE_MAXSTALLS P2/P3: stall budget: failed reps after which a leaf (that has run MINREPS) stops (6); twice this stops it regardless
//	CELERIS_PROBE_SIZES     P2/P3: body sizes in MiB, e.g. 4,64 (4,64)
//	CELERIS_PROBE_IDLE_MS   P2/P3: the no-byte rule, ms (5000, the celeris tests' idleCap761)
//	CELERIS_PROBE_SNAP_MS   P2/P3: first stall-time capture after this long without a byte, ms (2000; 0 = off)
//	CELERIS_PROBE_SNAP_GAP_MS P2/P3: second capture this long after the first, ms (500)
//	CELERIS_PROBE_SLOW_MS   P2/P3: a rep whose longest wait reaches this counts as stalled even if it passed (1000)
//	CELERIS_PROBE_ROUNDS    P3: every (case, engine) cell runs this many times in a fresh child each, ABBA order, REPS split between them (2)
//	CELERIS_PROBE_CASES     P3: cases, from all,nopin,fast8,fast4,slow4 (all five, in this order)
//	CELERIS_PROBE_ENGINES   P3: engines, from epoll,io_uring,adaptive,std (all four)
//	CELERIS_PROBE_SHAPES    P3: handler shapes, from sync,async-loop,async-route (sync,async-route)
//	CELERIS_PROBE_INJECT_MS P2/P3 proof knob, off by default: every INJECT_EVERY-th /big handler sleeps this long first
//	CELERIS_PROBE_INJECT_EVERY (5)
//	CELERIS_PROBE_INJECT_LOOPMISS P3 proof knob, off by default: 1 = discovery pretends to miss one loop thread (a moving case must then FAIL as mislabelled)
package stallprobe

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const childEnv = "CELERIS_PROBE_CHILD"

func TestMain(m *testing.M) {
	if kind := os.Getenv(childEnv); kind != "" {
		os.Exit(runChildMain(kind))
	}
	os.Exit(m.Run())
}

func runChildMain(kind string) int {
	var res any
	var err error
	if kind == "p2" || kind == "p3" {
		// A child outlives a parent that was killed (go test -timeout): end
		// with it, so no server keeps serving after the run is over.
		go func() {
			ppid := os.Getppid()
			for {
				time.Sleep(time.Second)
				if os.Getppid() != ppid {
					os.Exit(3)
				}
			}
		}()
	}
	switch kind {
	case "p1spin", "p1sleep":
		res, err = p1Child(kind)
	case "p2":
		res, err = p2Child()
	case "p3":
		res, err = p3Child()
	default:
		err = fmt.Errorf("unknown child kind %q", kind)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "child %s: %v\n", kind, err)
		return 1
	}
	if err := json.NewEncoder(os.Stdout).Encode(res); err != nil {
		fmt.Fprintf(os.Stderr, "child %s: encode: %v\n", kind, err)
		return 1
	}
	return 0
}

// ---- knobs ----------------------------------------------------------------

func envInt(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envList(name, def string) []string {
	v := os.Getenv(name)
	if v == "" {
		v = def
	}
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func envInt64(name string, def int64) int64 {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}

// ---- clock ----------------------------------------------------------------

// clk reads the system-wide CLOCK_MONOTONIC in nanoseconds, so timestamps of
// different processes compare. Go's monotonic clock is CLOCK_MONOTONIC read
// through the vDSO; one calibration against clock_gettime(2) gives the
// absolute value without a system call per read.
type clk struct {
	base time.Time
	abs  int64
}

func newClk() clk {
	abs := nowAbs()
	return clk{base: time.Now(), abs: abs}
}

func (c clk) now() int64 { return c.abs + int64(time.Since(c.base)) }

func nowAbs() int64 {
	var ts unix.Timespec
	_ = unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts)
	return ts.Nano()
}

// ---- running a child ------------------------------------------------------

type childSpec struct {
	kind    string
	env     []string // NAME=VALUE, added to the parent's environment
	mask    []int    // CPUs the child is started on (nil: inherit)
	timeout time.Duration
}

// runChild starts the test binary as a child of the given kind and returns
// its stdout (one JSON document) and stderr. With a mask, the fork happens on
// a goroutine of its own, on a locked thread that takes the mask for the fork
// only: the child inherits it, and the thread gets its own mask back before it
// is unlocked. If the thread's mask cannot be restored, the goroutine returns
// STILL LOCKED, so the runtime retires the thread instead of handing it,
// pinned, to other goroutines (the defect class of celeris#905). The child is
// started and waited for either way, and the failure is reported.
func runChild(spec childSpec) (stdout, stderr []byte, err error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), spec.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe)
	cmd.Env = append(os.Environ(), childEnv+"="+spec.kind)
	cmd.Env = append(cmd.Env, spec.env...)
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	cmd.WaitDelay = 5 * time.Second

	var restoreErr error
	if len(spec.mask) > 0 {
		type forked struct{ start, restore error }
		done := make(chan forked, 1)
		go func() {
			runtime.LockOSThread()
			var prev unix.CPUSet
			if gerr := unix.SchedGetaffinity(0, &prev); gerr != nil {
				runtime.UnlockOSThread() // nothing was changed
				done <- forked{start: fmt.Errorf("sched_getaffinity: %w", gerr)}
				return
			}
			var f forked
			m := maskOf(spec.mask)
			if serr := unix.SchedSetaffinity(0, &m); serr != nil {
				f.start = fmt.Errorf("sched_setaffinity %v: %w", spec.mask, serr)
			} else {
				f.start = cmd.Start()
			}
			if rerr := unix.SchedSetaffinity(0, &prev); rerr != nil {
				f.restore = rerr
				done <- f
				return // still locked: the thread ends with this goroutine
			}
			runtime.UnlockOSThread()
			done <- f
		}()
		f := <-done
		err, restoreErr = f.start, f.restore
	} else {
		err = cmd.Start()
	}
	if err != nil {
		if restoreErr != nil {
			err = fmt.Errorf("%w (and the forking thread's affinity could not be restored: %v; the thread was retired)", err, restoreErr)
		}
		return nil, se.Bytes(), err
	}
	err = cmd.Wait()
	if restoreErr != nil {
		err = fmt.Errorf("the forking thread's affinity could not be restored (%v; the thread was retired); child: %v", restoreErr, err)
	}
	return so.Bytes(), se.Bytes(), err
}

func decodeChild(out []byte, v any) error {
	out = bytes.TrimSpace(out)
	if len(out) == 0 {
		return fmt.Errorf("the child wrote nothing to stdout")
	}
	return json.Unmarshal(out, v)
}

// ---- logging --------------------------------------------------------------

// logf writes through t.Logf, with every line made safe for the stress tally:
// a line that starts (after blanks) with "---" or "===" could be read as a
// test verdict or a run marker, so it is shifted.
func logf(t *testing.T, format string, args ...any) {
	t.Helper()
	t.Log(sanitize(fmt.Sprintf(format, args...)))
}

func sanitize(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		tl := strings.TrimLeft(l, " \t")
		if strings.HasPrefix(tl, "---") || strings.HasPrefix(tl, "===") {
			lines[i] = "| " + l
		}
	}
	return strings.Join(lines, "\n")
}

func tailLines(b []byte, n int) string {
	ls := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(ls) > n {
		ls = ls[len(ls)-n:]
	}
	return strings.Join(ls, "\n")
}

func ms(ns int64) string { return fmt.Sprintf("%.1f", float64(ns)/1e6) }

// ---- a responsiveness watchdog -------------------------------------------

// wdEvent is one oversleep of the watchdog goroutine.
type wdEvent struct {
	T    int64 // absolute CLOCK_MONOTONIC, ns, when the sleep ended
	Over int64 // ns slept beyond the request
}

type wdResult struct {
	Events []wdEvent // oversleeps of 20 ms or more
	MaxNs  int64
	Sleeps int64
}

// watchdog sleeps 1 ms in a loop on an ordinary (unpinned) goroutine and
// records every oversleep of 20 ms or more: a whole-process stall that does
// not touch the connection under test (a runtime or kernel pause) shows here,
// a stall confined to one connection does not.
type watchdog struct {
	stop chan struct{}
	done chan struct{}
	res  wdResult
}

func startWatchdog(c clk) *watchdog {
	w := &watchdog{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(w.done)
		for {
			select {
			case <-w.stop:
				return
			default:
			}
			t0 := c.now()
			time.Sleep(time.Millisecond)
			t1 := c.now()
			over := t1 - t0 - int64(time.Millisecond)
			w.res.Sleeps++
			if over > w.res.MaxNs {
				w.res.MaxNs = over
			}
			if over >= 20_000_000 && len(w.res.Events) < 5000 {
				w.res.Events = append(w.res.Events, wdEvent{T: t1, Over: over})
			}
		}
	}()
	return w
}

func (w *watchdog) Stop() wdResult {
	close(w.stop)
	<-w.done
	return w.res
}

// wdWithin summarises the watchdog events inside [s, e].
func wdWithin(w wdResult, s, e int64) (max int64, n50, n500 int) {
	for _, ev := range w.Events {
		if ev.T >= s && ev.T-ev.Over <= e {
			if ev.Over > max {
				max = ev.Over
			}
			if ev.Over >= 50_000_000 {
				n50++
			}
			if ev.Over >= 500_000_000 {
				n500++
			}
		}
	}
	return
}
