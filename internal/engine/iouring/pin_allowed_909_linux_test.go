//go:build linux

package iouring

import (
	"bytes"
	"io"
	"log/slog"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/platform"
	"github.com/goceleris/celeris/internal/platform/pintest"
	"github.com/goceleris/celeris/internal/resource"
)

// lockedBuf is a bytes.Buffer the engine's goroutines can log into.
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// start909 starts an io_uring engine with the given worker count.
func start909(t *testing.T, workers int, log io.Writer) (*Engine, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	e, cancel, done := startRingRetried662(t, func() (*Engine, error) {
		return New(resource.Config{
			Addr:      addr,
			Protocol:  engine.HTTP1,
			Resources: resource.Resources{Workers: workers},
			Logger:    slog.New(slog.NewTextHandler(log, nil)),
		}, respondingHandler{})
	})
	return e, func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Listen: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Fatal("the io_uring engine did not stop within 15s")
		}
	}
}

// recordRingCPUs makes every worker's ring setup record the CPU it was asked
// to give the SQPOLL thread (-1: no SQ_AFF), and returns the recorded list.
func recordRingCPUs(t *testing.T) func() []int {
	t.Helper()
	var (
		mu   sync.Mutex
		cpus []int
	)
	orig := newWorkerRing
	newWorkerRing = func(entries, flags, sqPollIdle uint32, cpuID int) (*Ring, error) {
		mu.Lock()
		cpus = append(cpus, cpuID)
		mu.Unlock()
		return orig(entries, flags, sqPollIdle, cpuID)
	}
	t.Cleanup(func() { newWorkerRing = orig })
	return func() []int {
		mu.Lock()
		defer mu.Unlock()
		out := slices.Clone(cpus)
		slices.Sort(out)
		return out
	}
}

// celeris#909: with the process restricted to a mask that leaves out its lowest
// CPU, every worker pins inside the mask, and so does the CPU its SQPOLL thread
// would get. The old code pinned worker 0 to CPU 0.
func TestWorkersPinInsideTheAllowedSet909(t *testing.T) {
	if !pintest.InOwnProcess(t) {
		pintest.RunInOwnProcessOn(t, 2*time.Minute, pintest.MaskWithoutTheLowestCPU(t))
		return
	}
	ringCPUs := recordRingCPUs(t)
	pintest.LoopsStayInsideTheMask(t, "io_uring", func(t *testing.T) (int, func()) {
		e, stop := start909(t, 4, io.Discard)
		return e.NumWorkers(), stop
	})
	members, _ := pintest.AllowedCPUs()
	for _, c := range ringCPUs() {
		if !slices.Contains(members, c) {
			t.Errorf("celeris#909: a ring was set up with SQPOLL CPU %d, outside the process's mask %v", c, members)
		}
	}
}

// A plan that leaves workers unpinned is honoured: those workers keep the
// process's mask, report CPU -1, ask for no SQPOLL affinity, and the engine says
// why in one line. The plan alternates a pinned and an unpinned worker; an
// engine that RLIMIT_MEMLOCK holds to one worker (the CI runners' 8 MiB) gets
// the plan "unpinned" for it.
func TestUnpinnedWorkersStayUnpinned909(t *testing.T) {
	if !pintest.InOwnProcess(t) {
		pintest.RunInOwnProcess(t, 2*time.Minute)
		return
	}
	cpus, err := pintest.AllowedCPUs()
	if err != nil || len(cpus) < 3 {
		t.Skipf("needs 3 allowed CPUs, have %v (%v)", cpus, err)
	}
	base, _ := pintest.StartupMask()
	old := planWorkerCPUs
	var want []int
	planWorkerCPUs = func(n int) platform.CPUPlan {
		want = make([]int, n)
		for i := range want {
			want[i] = -1
			if i%2 == 0 && n > 1 {
				want[i] = cpus[len(cpus)-1-i/2]
			}
		}
		return platform.CPUPlan{CPUs: slices.Clone(want), Allowed: cpus, Heterogeneous: true,
			Note: "test plan: every other worker left unpinned"}
	}
	defer func() { planWorkerCPUs = old }()
	ringCPUs := recordRingCPUs(t)

	var log lockedBuf
	e, stop := start909(t, 4, &log)
	defer stop()
	n := e.NumWorkers()
	if n != len(want) {
		t.Fatalf("the engine runs %d worker(s), the plan was for %d", n, len(want))
	}
	pinned := 0
	for i, c := range want {
		if got := e.workers[i].CPUID(); got != c {
			t.Errorf("worker %d: CPUID() = %d, want %d", i, got, c)
		}
		if c >= 0 {
			pinned++
		}
	}
	wantRings := slices.Clone(want)
	slices.Sort(wantRings)
	if got := ringCPUs(); !slices.Equal(got, wantRings) {
		t.Errorf("SQPOLL CPUs asked of the rings: %v, want %v", got, wantRings)
	}
	running, err := pintest.Census()
	if err != nil {
		t.Fatal(err)
	}
	var single []string
	for _, th := range pintest.Off(running, base) {
		if th.Single() {
			single = append(single, th.CPUs)
		}
	}
	if len(single) != pinned {
		t.Errorf("%d thread(s) pinned to one CPU while %d of %d workers are planned pinned (the others must keep mask %s): %v",
			len(single), pinned, n, base, pintest.Off(running, base))
	}
	if k := strings.Count(log.String(), "capability-aware CPU pinning"); k != 1 {
		t.Errorf("%d info line(s) about the plan, want exactly 1:\n%s", k, log.String())
	}
	if strings.Contains(log.String(), "could not pin") {
		t.Errorf("an unpinned-by-plan worker was reported as a pin failure:\n%s", log.String())
	}
	t.Logf("celeris909 RESULT engine=io_uring plan=%v workers=%d pinned_threads=%d ring_cpus=%v", want, n, len(single), wantRings)
}

// A pin the kernel refuses is logged once for the engine, the worker reports
// that it is not pinned and its ring asks for no SQPOLL affinity, and the
// engine still serves.
func TestFailedPinIsLoggedOnceAndNotPretended909(t *testing.T) {
	if !pintest.InOwnProcess(t) {
		pintest.RunInOwnProcess(t, 2*time.Minute)
		return
	}
	base, _ := pintest.StartupMask()
	old := pinThreadToCPU
	pinThreadToCPU = func(int) error { return unix.EINVAL }
	defer func() { pinThreadToCPU = old }()
	ringCPUs := recordRingCPUs(t)

	var log lockedBuf
	e, stop := start909(t, 3, &log)
	defer stop()
	n := e.NumWorkers()
	for i := range n {
		if got := e.workers[i].CPUID(); got != -1 {
			t.Errorf("worker %d: CPUID() = %d after a failed pin, want -1", i, got)
		}
	}
	for _, c := range ringCPUs() {
		if c != -1 {
			t.Errorf("a ring was set up with SQPOLL CPU %d although the worker's pin failed", c)
		}
	}
	out := log.String()
	if k := strings.Count(out, "could not pin their thread"); k != 1 {
		t.Errorf("%d warning(s) about the failed pins, want exactly 1:\n%s", k, out)
	}
	if !strings.Contains(out, "failed="+strconv.Itoa(n)) || !strings.Contains(out, "invalid argument") || !strings.Contains(out, "allowed=") {
		t.Errorf("the warning should name the %d failures, the error and the allowed set:\n%s", n, out)
	}
	running, err := pintest.Census()
	if err != nil {
		t.Fatal(err)
	}
	if off := pintest.Off(running, base); len(off) != 0 {
		t.Errorf("%d thread(s) off the mask %s although every pin failed: %v", len(off), base, off)
	}
	conn, err := net.DialTimeout("tcp", e.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatalf("the engine does not accept after failed pins: %v", err)
	}
	_ = conn.Close()
}
