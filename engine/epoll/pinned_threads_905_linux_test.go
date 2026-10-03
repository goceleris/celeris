//go:build linux

package epoll

import (
	"context"
	"io"
	"log/slog"
	"net"
	"runtime"
	"testing"
	"time"

	celerisengine "github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/internal/platform/pintest"
	"github.com/goceleris/celeris/resource"
)

// celeris#905: a loop pins its OS thread to one CPU (Loop.run). CPU affinity
// belongs to the thread, not the goroutine, and the loop used to
// UnlockOSThread on the way out without undoing the pin, so a stopped engine
// handed every one of its loop threads back to the Go scheduler still pinned,
// and arbitrary goroutines ran on them from then on: after any Shutdown in a
// process that keeps running, a restart, or the next test in a test binary.
//
// The test runs in a process of its own (pintest.RunInOwnProcess), because
// its census is of every thread in the process and the other tests in this
// package leave threads of their own behind.
func TestStoppedEngineLeavesNoPinnedThread(t *testing.T) {
	if !pintest.InOwnProcess(t) {
		pintest.RunInOwnProcess(t, 2*time.Minute)
		return
	}
	pintest.StoppedEnginesLeaveNoPinnedThread(t, "epoll", 2, startEpoll905)
}

func startEpoll905(t *testing.T) (int, func()) {
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
		Resources: resource.Resources{Workers: min(runtime.NumCPU(), 4)},
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
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
		time.Sleep(5 * time.Millisecond)
	}
	stop := func() {
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
	return eng.NumWorkers(), stop
}
