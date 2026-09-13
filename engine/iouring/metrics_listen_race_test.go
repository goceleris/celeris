//go:build linux

package iouring

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/resource"
)

// TestMetricsDuringListenIsRaceFree pins celeris#578. Listen starts the
// worker goroutines before it assigns e.workers, so Metrics (public,
// reachable from any handler through Server.EngineInfo) can read the
// slice while Listen writes it. probatorium's first -race matrix run
// reported that pair in 7 of 16 io_uring cells. This test hammers
// Metrics from the moment Listen is called until the count is
// published; under -race the old Metrics fails, the locked one does not.
func TestMetricsDuringListenIsRaceFree(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	cfg := resource.Config{
		Addr:      addr,
		Protocol:  engine.HTTP1,
		Resources: resource.Resources{Workers: 4},
	}
	e, err := New(cfg, &asyncChurnHandler{})
	if err != nil {
		t.Skipf("iouring engine unavailable: %v", err)
	}

	var stop atomic.Bool
	reads := make(chan int64, 1)
	go func() {
		var n int64
		for !stop.Load() {
			_ = e.Metrics()
			n++
		}
		reads <- n
	}()

	ctx, cancel := context.WithCancel(t.Context())
	errCh := make(chan error, 1)
	go func() { errCh <- e.Listen(ctx) }()
	defer func() {
		cancel()
		select {
		case <-errCh:
		case <-time.After(3 * time.Second):
		}
	}()

	// Wait for the count to be published, whatever it is: CI caps io_uring
	// to a single worker through RLIMIT_MEMLOCK, so the resolved count is
	// not the requested one, and the race is the same with one worker.
	deadline := time.Now().Add(5 * time.Second)
	for e.Metrics().Workers == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("workers never published: %+v", e.Metrics())
		}
		time.Sleep(time.Millisecond)
	}
	stop.Store(true)
	if n := <-reads; n == 0 {
		t.Fatal("the reader never ran")
	}
}
