//go:build linux

package epoll

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	celerisengine "github.com/goceleris/celeris/internal/engine"
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

// start909 starts an epoll engine with the given worker count, logging into
// log, and returns it with its stop function.
func start909(t *testing.T, workers int, log *lockedBuf) (*Engine, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	eng, err := New(resource.Config{
		Addr:      addr,
		Protocol:  celerisengine.HTTP1,
		Resources: resource.Resources{Workers: workers},
		Logger:    slog.New(slog.NewTextHandler(log, nil)),
	}, respondingHandler{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- eng.Listen(ctx) }()
	for deadline := time.Now().Add(10 * time.Second); eng.Addr() == nil; {
		select {
		case err := <-done:
			cancel()
			t.Fatalf("Listen returned before it bound: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("the epoll engine did not bind within 10s")
		}
		time.Sleep(time.Millisecond)
	}
	return eng, func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Listen: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Fatal("the epoll engine did not stop within 15s")
		}
	}
}

// celeris#909: with the process restricted to a mask that leaves out its lowest
// CPU (as taskset -c 1-N does), every loop pins inside the mask. The old code
// pinned loop 0 to CPU 0.
func TestLoopsPinInsideTheAllowedSet909(t *testing.T) {
	if !pintest.InOwnProcess(t) {
		pintest.RunInOwnProcessOn(t, 2*time.Minute, pintest.MaskWithoutTheLowestCPU(t))
		return
	}
	pintest.LoopsStayInsideTheMask(t, "epoll", func(t *testing.T) (int, func()) {
		return startEpoll905(t)
	})
}

// A plan that leaves loops unpinned (a big.LITTLE host's little CPUs) is
// honoured: those loops keep the process's mask, report CPU -1, and the engine
// says why in one line.
func TestUnpinnedLoopsStayUnpinned909(t *testing.T) {
	if !pintest.InOwnProcess(t) {
		pintest.RunInOwnProcess(t, 2*time.Minute)
		return
	}
	cpus, err := pintest.AllowedCPUs()
	if err != nil || len(cpus) < 3 {
		t.Skipf("needs 3 allowed CPUs, have %v (%v)", cpus, err)
	}
	base, _ := pintest.StartupMask()
	a, b := cpus[len(cpus)-1], cpus[len(cpus)-2]
	old := planWorkerCPUs
	planWorkerCPUs = func(n int) platform.CPUPlan {
		return platform.CPUPlan{CPUs: []int{a, -1, b, -1}[:n], Allowed: cpus, Heterogeneous: true,
			Note: "test plan: two loops left unpinned"}
	}
	defer func() { planWorkerCPUs = old }()

	var log lockedBuf
	eng, stop := start909(t, 4, &log)
	defer stop()
	for i, want := range []int{a, -1, b, -1} {
		if got := eng.loops[i].CPUID(); got != want {
			t.Errorf("loop %d: CPUID() = %d, want %d", i, got, want)
		}
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
	if len(single) != 2 {
		t.Errorf("%d thread(s) pinned to one CPU while 2 loops are planned pinned (the others must keep mask %s): %v",
			len(single), base, pintest.Off(running, base))
	}
	if n := strings.Count(log.String(), "capability-aware CPU pinning"); n != 1 {
		t.Errorf("%d info line(s) about the plan, want exactly 1:\n%s", n, log.String())
	}
	if strings.Contains(log.String(), "could not pin") {
		t.Errorf("an unpinned-by-plan loop was reported as a pin failure:\n%s", log.String())
	}
}

// A pin the kernel refuses is logged once for the engine, the loop reports
// that it is not pinned, and the engine still serves.
func TestFailedPinIsLoggedOnceAndNotPretended909(t *testing.T) {
	if !pintest.InOwnProcess(t) {
		pintest.RunInOwnProcess(t, 2*time.Minute)
		return
	}
	base, _ := pintest.StartupMask()
	old := pinThreadToCPU
	pinThreadToCPU = func(int) error { return unix.EINVAL }
	defer func() { pinThreadToCPU = old }()

	var log lockedBuf
	eng, stop := start909(t, 3, &log)
	defer stop()
	for i, l := range eng.loops {
		if got := l.CPUID(); got != -1 {
			t.Errorf("loop %d: CPUID() = %d after a failed pin, want -1", i, got)
		}
	}
	out := log.String()
	if n := strings.Count(out, "could not pin their thread"); n != 1 {
		t.Errorf("%d warning(s) about the failed pins, want exactly 1:\n%s", n, out)
	}
	if !strings.Contains(out, "failed=3") || !strings.Contains(out, "invalid argument") || !strings.Contains(out, "allowed=") {
		t.Errorf("the warning should name the 3 failures, the error and the allowed set:\n%s", out)
	}
	running, err := pintest.Census()
	if err != nil {
		t.Fatal(err)
	}
	if off := pintest.Off(running, base); len(off) != 0 {
		t.Errorf("%d thread(s) off the mask %s although every pin failed: %v", len(off), base, off)
	}
	conn, err := net.DialTimeout("tcp", (*eng.addr.Load()).String(), 5*time.Second)
	if err != nil {
		t.Fatalf("the engine does not accept after failed pins: %v", err)
	}
	_ = conn.Close()
}
