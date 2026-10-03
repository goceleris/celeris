//go:build linux

package adaptive

import (
	"context"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/internal/platform/pintest"
	"github.com/goceleris/celeris/resource"
)

// celeris#905 through the adaptive engine. Both sub-engines pin each loop
// thread to one CPU. A promote does not stop the outgoing epoll engine: its
// loops pause and park, each still locked to its own thread, so no pinned
// thread can reach the scheduler then, and the test checks that after the
// promote. The loops of both sub-engines exit when the adaptive engine
// stops, and each used to hand its thread back to the scheduler still pinned.
//
// The test runs in a process of its own (pintest.RunInOwnProcess), because
// its census is of every thread in the process and the other tests in this
// package leave threads of their own behind.
func TestStoppedAdaptiveEngineLeavesNoPinnedThread(t *testing.T) {
	if !pintest.InOwnProcess(t) {
		pintest.RunInOwnProcess(t, 2*time.Minute)
		return
	}
	pintest.StoppedEnginesLeaveNoPinnedThread(t, "adaptive", 2, startPromoted905)
}

// startPromoted905 starts an adaptive engine on epoll, promotes it to
// io_uring, waits for the epoll listeners to close (the outgoing loops then
// park), and checks that no goroutine that locks a thread is given a pinned
// one at that point.
func startPromoted905(t *testing.T) (int, func()) {
	t.Helper()
	base, err := pintest.ProcessMask()
	if err != nil {
		t.Fatalf("read the process's CPU mask: %v", err)
	}
	e, err := New(resource.Config{
		Addr:     "127.0.0.1:0",
		Protocol: engine.HTTP1,
		Logger:   slog.New(slog.DiscardHandler),
	}, respHandler{}, nil)
	if err != nil {
		skipOrFailUpswitch662(t, "adaptive.New unsupported here: %v", err)
	}
	// Only the test switches this engine. ForceSwitch bypasses the freeze.
	e.FreezeSwitching()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Listen(ctx) }()
	stop := func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Listen: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Fatal("the adaptive engine did not stop within 15s")
		}
	}
	for dl := time.Now().Add(10 * time.Second); e.Addr() == nil && time.Now().Before(dl); {
		time.Sleep(5 * time.Millisecond)
	}
	if e.Addr() == nil {
		stop()
		t.Fatal("the adaptive engine did not bind within 10s")
	}
	if got := e.ActiveEngine().Type(); got != engine.Epoll {
		stop()
		t.Fatalf("this test promotes from epoll, but the engine started on %v", got)
	}
	port := e.Addr().(*net.TCPAddr).Port
	epollSet, _ := buildStandby662(t, e, port)
	waitGone662(t, port, epollSet, "the outgoing epoll listeners after the promote")
	wE, wI := workers662(e)

	seen, err := pintest.LockedThreads(64)
	if err != nil {
		stop()
		t.Fatalf("locked-thread probe after the promote: %v", err)
	}
	var bad []pintest.Locked
	for _, s := range seen {
		if s.CPUs != base {
			bad = append(bad, s)
		}
	}
	t.Logf("after the promote (epoll loops %d, io_uring workers %d): %d of %d goroutines that locked a "+
		"thread got one off the process mask %s: %v", wE, wI, len(bad), len(seen), base, bad)
	if len(bad) > 0 {
		t.Errorf("after a promote, %d goroutine(s) that locked an OS thread got one pinned off the process "+
			"mask %s: the promote handed a pinned thread back to the scheduler: %v", len(bad), base, bad)
	}
	return wE + wI, stop
}
