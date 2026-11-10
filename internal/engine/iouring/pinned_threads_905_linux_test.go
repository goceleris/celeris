//go:build linux

package iouring

import (
	"context"
	"io"
	"log/slog"
	"net"
	"runtime"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/platform/pintest"
	"github.com/goceleris/celeris/internal/resource"
)

// celeris#905: a worker pins its OS thread to one CPU (Worker.run). CPU
// affinity belongs to the thread, not the goroutine, and the worker used to
// UnlockOSThread on the way out without undoing the pin, so a stopped engine
// handed every one of its worker threads back to the Go scheduler still
// pinned, and arbitrary goroutines ran on them from then on: after any
// Shutdown in a process that keeps running, a restart, or the next test in a
// test binary.
//
// The test runs in a process of its own (pintest.RunInOwnProcess), because
// its census is of every thread in the process and the other tests in this
// package leave threads of their own behind. At the CI runner's 8 MiB memlock
// the engine runs one worker, which is enough: one pinned thread is what the
// census has to find gone.
func TestStoppedEngineLeavesNoPinnedThread(t *testing.T) {
	if !pintest.InOwnProcess(t) {
		pintest.RunInOwnProcess(t, 2*time.Minute)
		return
	}
	pintest.StoppedEnginesLeaveNoPinnedThread(t, "io_uring", 2, startIOUring905)
}

// TestStoppedEngineRestoresTheMainThreadMask890 is the celeris#905 restore on
// the one thread it is for, every run (celeris#890 item 42). A worker's thread
// exits with its goroutine, except the main thread, which cannot: it is parked
// for good, and its CPU mask is what /proc/<pid>/status and taskset -p report
// for the whole process, so Worker.run puts the mask it saved back on the way
// out. TestStoppedEngineLeavesNoPinnedThread checks that only in the runs in
// which the scheduler put a worker on the main thread (8 of 20 first cycles on
// a laptop, none in CI run 37087402233). This test puts the first worker there
// on purpose (mainThreadSeam890, served by the main goroutine of a child
// started by pintest.RunInOwnProcessOnMainThread), asserts that the main
// thread is pinned while the engine runs, which is the premise, and that it
// has the mask the process started with once the engine has stopped.
func TestStoppedEngineRestoresTheMainThreadMask890(t *testing.T) {
	if !pintest.InOwnProcess(t) {
		pintest.RunInOwnProcessOnMainThread(t, 2*time.Minute)
		return
	}
	base, err := pintest.StartupMask()
	if err != nil {
		t.Fatalf("read the process's CPU mask at init: %v", err)
	}
	if n := pintest.CountCPUs(base); n < 2 {
		t.Skipf("the process may run on CPU %s only (%d CPU): a pinned thread cannot be told from one that is not", base, n)
	}
	seam := pintest.RunOnMainThread
	mainThreadSeam890.Store(&seam)
	t.Cleanup(func() { mainThreadSeam890.Store(nil) })

	_, stop := startIOUring905(t)
	var running string
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		running, err = pintest.ProcessMask()
		if err != nil {
			stop()
			t.Fatalf("read the main thread's mask: %v", err)
		}
		if running != base {
			break
		}
		if time.Now().After(deadline) {
			stop()
			t.Fatalf("premise: the main thread still has the process mask %s 5 s after the engine started: no worker "+
				"ran on it, or its pin failed, and this test would check nothing", base)
		}
	}
	if n := pintest.CountCPUs(running); n != 1 {
		stop()
		t.Fatalf("premise: the main thread runs a worker but its mask is %s (%d CPUs), not one CPU", running, n)
	}
	stop()
	var after string
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if after, err = pintest.ProcessMask(); err != nil {
			t.Fatalf("read the main thread's mask: %v", err)
		}
		if after == base || time.Now().After(deadline) {
			break
		}
	}
	t.Logf("celeris890 MAINTHREAD process_mask=%s main_thread_while_running=%s main_thread_after_stop=%s", base, running, after)
	if after != base {
		t.Errorf("celeris#905: after the engine stopped, the main thread, which hosted worker 0, has CPU mask %s, "+
			"not the %s the process started with: Worker.run did not restore the mask it saved", after, base)
	}
	if off := pintest.Off(mustCensus890(t), base); len(off) > 0 {
		t.Errorf("celeris#905: %d thread(s) are off the process mask %s after the engine stopped: %v", len(off), base, off)
	}
}

func mustCensus890(t *testing.T) []pintest.Thread {
	t.Helper()
	th, err := pintest.Census()
	if err != nil {
		t.Fatalf("census: %v", err)
	}
	return th
}

// BenchmarkListenStopCycle905 is what the celeris#905 fix costs a restart.
// A stopped worker's thread now exits instead of going back to the scheduler,
// so the next start has the runtime create its worker threads rather than
// reuse idle ones. One op is New, Listen until bound, cancel, and Listen's
// return. Run it with an unlimited memlock: at 8 MiB back-to-back starts wait
// for the kernel to uncharge closed rings (ring_budget_linux_test.go).
func BenchmarkListenStopCycle905(b *testing.B) {
	for b.Loop() {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			b.Fatalf("pick port: %v", err)
		}
		addr := ln.Addr().String()
		_ = ln.Close()
		e, err := New(resource.Config{
			Addr:      addr,
			Protocol:  engine.HTTP1,
			Resources: resource.Resources{Workers: min(runtime.NumCPU(), 4)},
			Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		}, respondingHandler{})
		if err != nil {
			b.Fatalf("New: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- e.Listen(ctx) }()
		for e.Addr() == nil || e.NumWorkers() == 0 {
			select {
			case err := <-done:
				cancel()
				b.Fatalf("Listen returned before it bound: %v", err)
			default:
			}
			time.Sleep(50 * time.Microsecond)
		}
		cancel()
		if err := <-done; err != nil {
			b.Fatalf("Listen: %v", err)
		}
	}
}

func startIOUring905(t *testing.T) (int, func()) {
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
			Resources: resource.Resources{Workers: min(runtime.NumCPU(), 4)},
			Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		}, respondingHandler{})
	})
	stop := func() {
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
	return e.NumWorkers(), stop
}
