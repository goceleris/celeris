//go:build linux

package iouring

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/resource"
)

// celeris#895: the port Listen decides for a port 0 is held from the moment
// it is decided until Listen returns, on every path out of it: a socket that
// never listens, so no other socket can take the port while the workers start,
// while accepting is paused (the workers then have no listen socket and bind the
// port again on a resume), or at any time in between. The workers' own sockets
// join the port's SO_REUSEPORT group beside it, and it is closed last, after
// they have been closed, so the port is free the instant Listen returns
// (celeris#896).

// newEngine895 is New, with a start that fails only on ring ENOMEM retried
// (the engine's probes build rings, and at the runner's 8 MiB the rings of
// the test before are uncharged asynchronously, ring_budget_linux_test.go).
func newEngine895(t *testing.T, addr string, workers int) *Engine {
	t.Helper()
	var e *Engine
	_, _, err := retryRingENOMEM(func() error {
		var nerr error
		e, nerr = New(resource.Config{
			Addr:      addr,
			Engine:    engine.IOUring,
			Protocol:  engine.HTTP1,
			Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
			Resources: resource.Resources{Workers: workers},
		}, respondingHandler{})
		return nerr
	})
	if err != nil {
		if isRingENOMEM(err) {
			skipUnlessIOUringUnusable(t, err)
		}
		skipOrFail656(t, "New: %v", err)
	}
	return e
}

func newPort0Engine895(t *testing.T, workers int) *Engine {
	t.Helper()
	return newEngine895(t, "127.0.0.1:0", workers)
}

// TestPort0IsHeldBetweenBeingChosenAndBeingBound895: while the hold is held
// (the hook runs before any worker is created) a plain net.Listen on the chosen
// address is refused, so nothing but the engine's own sockets can take it; and
// while Listen is serving the hold stays beside the workers' sockets (it keeps
// the port through a pause, TestPort0PausedPortCannotBeTaken895), and Listen
// gives it back as it returns.
func TestPort0IsHeldBetweenBeingChosenAndBeingBound895(t *testing.T) {
	var hookErr atomic.Value
	var chosen atomic.Value
	portHoldHook = func(addr string) {
		chosen.Store(addr)
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			_ = ln.Close()
			hookErr.Store("a plain net.Listen on " + addr + " succeeded while the port was held")
			return
		}
		if !errors.Is(err, unix.EADDRINUSE) {
			hookErr.Store("a plain net.Listen on " + addr + " failed with " + err.Error() + ", want EADDRINUSE")
		}
	}
	t.Cleanup(func() { portHoldHook = nil })

	r := start895(t, "127.0.0.1:0", 2, 1, false)
	r.e.mu.Lock()
	workers := len(r.e.workers)
	r.e.mu.Unlock()
	if msg, _ := hookErr.Load().(string); msg != "" {
		t.Fatal(msg)
	}
	addr, _ := chosen.Load().(string)
	if addr == "" {
		t.Fatal("the hook never ran: Listen did not resolve the port")
	}
	if got := r.e.Addr().String(); got != addr {
		t.Fatalf("Addr() = %s, but the port chosen before the workers bound was %s", got, addr)
	}
	port := portOf895(t, r.e.Addr())
	c := census895(t)
	if n := c.listening[port]; n != workers {
		t.Fatalf("%d sockets listen on port %d with the engine serving, want %d (one per worker)", n, port, workers)
	}
	if n := c.bound[port]; n != workers+1 {
		t.Fatalf("%d sockets bound to port %d with the engine serving, want %d (one per worker and the hold)", n, port, workers+1)
	}
	if err := r.stop(t); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	if n := census895(t).bound[port]; n != 0 {
		t.Fatalf("%d sockets still bound to port %d after Listen returned, want 0 (the hold included)", n, port)
	}
}

// TestPort0HoldIsReleasedWhenListenFails895: a Listen that fails after the
// port was chosen gives it back.
func TestPort0HoldIsReleasedWhenListenFails895(t *testing.T) {
	orig := listenAddrOf
	listenAddrOf = func(int) net.Addr { return nil }
	t.Cleanup(func() { listenAddrOf = orig })
	var chosen atomic.Value
	portHoldHook = func(addr string) { chosen.Store(addr) }
	t.Cleanup(func() { portHoldHook = nil })

	var err error
	_, _, _ = retryRingENOMEM(func() error {
		e := newPort0Engine895(t, 2)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- e.Listen(ctx) }()
		select {
		case err = <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("Listen kept running with no address to publish")
		}
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "no worker could report") {
		t.Fatalf("Listen = %v, want the no-address error", err)
	}
	addr, _ := chosen.Load().(string)
	if addr == "" {
		t.Fatal("the hook never ran")
	}
	port := portOfString895(t, addr)
	if n := census895(t).bound[port]; n != 0 {
		t.Fatalf("%d sockets still bound to port %d after Listen failed, want 0", n, port)
	}
	// bindWithin656, not an immediate bind: the sockets of a worker that
	// was cancelled are let go of asynchronously on a main without the
	// celeris#896 fix, which this test is not about.
	if err := bindWithin656(addr, 3*time.Second); err != nil {
		t.Fatalf("net.Listen on %s after the failed Listen: %v", addr, err)
	}
}

// TestPort0ListenErrorWhenTheHoldCannotBeBound895: an address this host does
// not own is an error from Listen, not workers that each fail or bind alone.
func TestPort0ListenErrorWhenTheHoldCannotBeBound895(t *testing.T) {
	e := newEngine895(t, "203.0.113.77:0", 2)
	_, _, err := retryRingENOMEM(func() error { return e.Listen(t.Context()) })
	if err == nil || !errors.Is(err, unix.EADDRNOTAVAIL) {
		t.Fatalf("Listen on an address this host does not own = %v, want EADDRNOTAVAIL", err)
	}
	t.Logf("Listen: %v", err)
}

func portOfString895(t *testing.T, addr string) int {
	t.Helper()
	return portOf895(t, mustTCPAddr895(t, addr))
}

func mustTCPAddr895(t *testing.T, addr string) net.Addr {
	t.Helper()
	a, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		t.Fatalf("resolve %q: %v", addr, err)
	}
	return a
}
