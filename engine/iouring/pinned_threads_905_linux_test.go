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

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/internal/platform/pintest"
	"github.com/goceleris/celeris/resource"
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
